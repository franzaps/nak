package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/nbd-wtf/go-nostr"
)

func performMusig(
	ctx context.Context,
	sec string,
	evt *nostr.Event,
	numSigners int,
	keys []string,
	nonces []string,
	secNonce string,
	partialSigs []string,
) (signed bool, err error) {
	// preprocess data received
	secb, err := hex.DecodeString(sec)
	if err != nil {
		return false, err
	}
	seck, pubk := btcec.PrivKeyFromBytes(secb)

	knownSigners := make([]*btcec.PublicKey, 0, numSigners)
	includesUs := false
	for _, hexpub := range keys {
		bpub, err := hex.DecodeString(hexpub)
		if err != nil {
			return false, err
		}
		spub, err := btcec.ParsePubKey(bpub)
		if err != nil {
			return false, err
		}
		knownSigners = append(knownSigners, spub)

		if spub.IsEqual(pubk) {
			includesUs = true
		}
	}
	if !includesUs {
		knownSigners = append(knownSigners, pubk)
	}

	knownNonces := make([][66]byte, 0, numSigners)
	for _, hexnonce := range nonces {
		bnonce, err := hex.DecodeString(hexnonce)
		if err != nil {
			return false, err
		}
		if len(bnonce) != 66 {
			return false, fmt.Errorf("nonce is not 66 bytes: %s", hexnonce)
		}
		var b66nonce [66]byte
		copy(b66nonce[:], bnonce)
		knownNonces = append(knownNonces, b66nonce)
	}

	knownPartialSigs := make([]*musig2.PartialSignature, 0, numSigners)
	for _, hexps := range partialSigs {
		bps, err := hex.DecodeString(hexps)
		if err != nil {
			return false, err
		}
		var ps musig2.PartialSignature
		if err := ps.Decode(bytes.NewBuffer(bps)); err != nil {
			return false, fmt.Errorf("invalid partial signature %s: %w", hexps, err)
		}
		knownPartialSigs = append(knownPartialSigs, &ps)
	}

	// create the context
	var mctx *musig2.Context
	if len(knownSigners) < numSigners {
		// we don't know all the signers yet
		mctx, err = musig2.NewContext(seck, true,
			musig2.WithNumSigners(numSigners),
			musig2.WithEarlyNonceGen(),
		)
		if err != nil {
			return false, fmt.Errorf("failed to create signing context with %d unknown signers: %w",
				numSigners, err)
		}
	} else {
		// we know all the signers
		mctx, err = musig2.NewContext(seck, true,
			musig2.WithKnownSigners(knownSigners),
		)
		if err != nil {
			return false, fmt.Errorf("failed to create signing context with %d known signers: %w",
				len(knownSigners), err)
		}
	}

	// nonce generation phase -- for sharing
	if len(knownSigners) < numSigners {
		// if we don't have all the signers we just generate a nonce and yield it to the next people
		nonce, err := mctx.EarlySessionNonce()
		if err != nil {
			return false, err
		}
		fmt.Fprintf(os.Stderr, "the following code should be saved secretly until the next step an included with --musig2-nonce-secret:\n")
		fmt.Fprintf(os.Stderr, "%s\n\n", base64.StdEncoding.EncodeToString(nonce.SecNonce[:]))

		knownNonces = append(knownNonces, nonce.PubNonce)
		printPublicCommandForNextPeer(evt, numSigners, knownSigners, knownNonces, nil, false)
		return false, nil
	}

	// if we got here we have all the pubkeys, so we can print the combined key
	if comb, err := mctx.CombinedKey(); err != nil {
		return false, fmt.Errorf("failed to combine keys (after %d signers): %w", len(knownSigners), err)
	} else {
		fmt.Fprintf(os.Stderr, "combined key: %x\n\n", comb.SerializeCompressed())
	}

	// we have all the signers, which means we must also have all the nonces
	var session *musig2.Session
	if len(keys) == numSigners-1 {
		// if we were the last to include our key, that means we have to include our nonce here to
		// i.e. we didn't input our own pub nonce in the parameters
		session, err = mctx.NewSession()
		if err != nil {
			return false, fmt.Errorf("failed to create session as the last peer to include our key: %w", err)
		}
		knownNonces = append(knownNonces, session.PublicNonce())
	} else {
		// otherwise we have included our own nonce in the parameters (from copypasting) but must
		// also include the secret nonce that wasn't shared with peers
		if secNonce == "" {
			return false, fmt.Errorf("missing --musig2-nonce-secret value")
		}
		secNonceB, err := base64.StdEncoding.DecodeString(secNonce)
		if err != nil {
			return false, fmt.Errorf("invalid --musig2-nonce-secret: %w", err)
		}
		var secNonce97 [97]byte
		copy(secNonce97[:], secNonceB)
		session, err = mctx.NewSession(musig2.WithPreGeneratedNonce(&musig2.Nonces{
			SecNonce: secNonce97,
			PubNonce: secNonceToPubNonce(secNonce97),
		}))
		if err != nil {
			return false, fmt.Errorf("failed to create signing session with secret nonce: %w", err)
		}
	}

	var noncesOk bool
	for _, b66nonce := range knownNonces {
		if b66nonce == session.PublicNonce() {
			// don't add our own nonce
			continue
		}

		noncesOk, err = session.RegisterPubNonce(b66nonce)
		if err != nil {
			return false, fmt.Errorf("failed to register nonce: %w", err)
		}
	}
	if !noncesOk {
		return false, fmt.Errorf("we've registered all the nonces we had but at least one is missing, this shouldn't happen")
	}

	// signing phase
	// we always have to sign, so let's do this
	id := evt.GetID()
	hash, _ := hex.DecodeString(id)
	var msg32 [32]byte
	copy(msg32[:], hash)
	partialSig, err := session.Sign(msg32) // this will already include our sig in the bundle
	if err != nil {
		return false, fmt.Errorf("failed to produce partial signature: %w", err)
	}

	if len(knownPartialSigs)+1 < len(knownSigners) {
		// still missing some signatures
		knownPartialSigs = append(knownPartialSigs, partialSig) // we include ours here just so it's printed
		printPublicCommandForNextPeer(evt, numSigners, knownSigners, knownNonces, knownPartialSigs, true)
		return false, nil
	} else {
		// we have all signatures
		for _, ps := range knownPartialSigs {
			_, err = session.CombineSig(ps)
			if err != nil {
				return false, fmt.Errorf("failed to combine partial signature: %w", err)
			}
		}
	}

	// we have the signature
	evt.Sig = hex.EncodeToString(session.FinalSig().Serialize())

	return true, nil
}

func printPublicCommandForNextPeer(
	evt *nostr.Event,
	numSigners int,
	knownSigners []*btcec.PublicKey,
	knownNonces [][66]byte,
	knownPartialSigs []*musig2.PartialSignature,
	includeNonceSecret bool,
) {
	maybeNonceSecret := ""
	if includeNonceSecret {
		maybeNonceSecret = " --musig2-nonce-secret '<their-nonce-secret>'"
	}

	fmt.Fprintf(os.Stderr, "the next signer and they should call this on their side:\nnak event --sec <their-key> --musig2 %d %s%s%s%s%s\n",
		numSigners,
		eventToCliArgs(evt),
		signersToCliArgs(knownSigners),
		noncesToCliArgs(knownNonces),
		partialSigsToCliArgs(knownPartialSigs),
		maybeNonceSecret,
	)
}

func eventToCliArgs(evt *nostr.Event) string {
	b := strings.Builder{}
	b.Grow(100)

	b.WriteString("-k ")
	b.WriteString(strconv.Itoa(evt.Kind))

	b.WriteString(" -ts ")
	b.WriteString(strconv.FormatInt(int64(evt.CreatedAt), 10))

	b.WriteString(" -c '")
	b.WriteString(evt.Content)
	b.WriteString("'")

	for _, tag := range evt.Tags {
		b.WriteString(" -t '")
		b.WriteString(tag.Key())
		if len(tag) > 1 {
			b.WriteString("=")
			b.WriteString(tag[1])
			if len(tag) > 2 {
				for _, item := range tag[2:] {
					b.WriteString(",")
					b.WriteString(item)
				}
			}
		}
		b.WriteString("'")
	}

	return b.String()
}

func signersToCliArgs(knownSigners []*btcec.PublicKey) string {
	b := strings.Builder{}
	b.Grow(len(knownSigners) * (17 + 66))

	for _, signerPub := range knownSigners {
		b.WriteString(" --musig2-pubkey ")
		b.WriteString(hex.EncodeToString(signerPub.SerializeCompressed()))
	}

	return b.String()
}

func noncesToCliArgs(knownNonces [][66]byte) string {
	b := strings.Builder{}
	b.Grow(len(knownNonces) * (16 + 132))

	for _, nonce := range knownNonces {
		b.WriteString(" --musig2-nonce ")
		b.WriteString(hex.EncodeToString(nonce[:]))
	}

	return b.String()
}

func partialSigsToCliArgs(knownPartialSigs []*musig2.PartialSignature) string {
	b := strings.Builder{}
	b.Grow(len(knownPartialSigs) * (18 + 64))

	for _, partialSig := range knownPartialSigs {
		b.WriteString(" --musig2-partial ")
		w := &bytes.Buffer{}
		partialSig.Encode(w)
		b.Write([]byte(hex.EncodeToString(w.Bytes())))
	}

	return b.String()
}

// this function is copied from btcec because it's not exported for some reason
func secNonceToPubNonce(secNonce [musig2.SecNonceSize]byte) [musig2.PubNonceSize]byte {
	var k1Mod, k2Mod btcec.ModNScalar
	k1Mod.SetByteSlice(secNonce[:btcec.PrivKeyBytesLen])
	k2Mod.SetByteSlice(secNonce[btcec.PrivKeyBytesLen:])

	var r1, r2 btcec.JacobianPoint
	btcec.ScalarBaseMultNonConst(&k1Mod, &r1)
	btcec.ScalarBaseMultNonConst(&k2Mod, &r2)

	// Next, we'll convert the key in jacobian format to a normal public
	// key expressed in affine coordinates.
	r1.ToAffine()
	r2.ToAffine()
	r1Pub := btcec.NewPublicKey(&r1.X, &r1.Y)
	r2Pub := btcec.NewPublicKey(&r2.X, &r2.Y)

	var pubNonce [musig2.PubNonceSize]byte

	// The public nonces are serialized as: R1 || R2, where both keys are
	// serialized in compressed format.
	copy(pubNonce[:], r1Pub.SerializeCompressed())
	copy(
		pubNonce[btcec.PubKeyBytesLenCompressed:],
		r2Pub.SerializeCompressed(),
	)

	return pubNonce
}
