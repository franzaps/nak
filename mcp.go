package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/nbd-wtf/go-nostr/sdk"
	"github.com/urfave/cli/v3"
)

var mcpServer = &cli.Command{
	Name:                      "mcp",
	Usage:                     "pander to the AI gods",
	Description:               ``,
	DisableSliceFlagSeparator: true,
	Flags:                     []cli.Flag{},
	Action: func(ctx context.Context, c *cli.Command) error {
		s := server.NewMCPServer(
			"nak",
			version,
		)

		s.AddTool(mcp.NewTool("publish_note",
			mcp.WithDescription("Publish a short note event to Nostr with the given text content"),
			mcp.WithString("content", mcp.Description("Arbitrary string to be published"), mcp.Required()),
			mcp.WithString("relay", mcp.Description("Relay to publish the note to")),
			mcp.WithString("mention", mcp.Description("Nostr user's public key to be mentioned")),
		), func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			content := required[string](r, "content")
			mention, _ := optional[string](r, "mention")
			relay, _ := optional[string](r, "relay")

			if mention != "" && !nostr.IsValidPublicKey(mention) {
				return mcp.NewToolResultError("the given mention isn't a valid public key, it must be 32 bytes hex, like the ones returned by search_profile"), nil
			}

			sk := os.Getenv("NOSTR_SECRET_KEY")
			if sk == "" {
				sk = "0000000000000000000000000000000000000000000000000000000000000001"
			}
			var relays []string

			evt := nostr.Event{
				Kind:      1,
				Tags:      nostr.Tags{{"client", "goose/nak"}},
				Content:   content,
				CreatedAt: nostr.Now(),
			}

			if mention != "" {
				evt.Tags = append(evt.Tags, nostr.Tag{"p", mention})
				// their inbox relays
				relays = sys.FetchInboxRelays(ctx, mention, 3)
			}

			evt.Sign(sk)

			// our write relays
			relays = append(relays, sys.FetchOutboxRelays(ctx, evt.PubKey, 3)...)

			if len(relays) == 0 {
				relays = []string{"nos.lol", "relay.damus.io"}
			}

			// extra relay specified
			if relay != "" {
				relays = append(relays, relay)
			}

			result := strings.Builder{}
			result.WriteString(
				fmt.Sprintf("the event we generated has id '%s', kind '%d' and is signed by pubkey '%s'. ",
					evt.ID,
					evt.Kind,
					evt.PubKey,
				),
			)

			for res := range sys.Pool.PublishMany(ctx, relays, evt) {
				if res.Error != nil {
					result.WriteString(
						fmt.Sprintf("there was an error publishing the event to the relay %s. ",
							res.RelayURL),
					)
				} else {
					result.WriteString(
						fmt.Sprintf("the event was successfully published to the relay %s. ",
							res.RelayURL),
					)
				}
			}

			return mcp.NewToolResultText(result.String()), nil
		})

		s.AddTool(mcp.NewTool("resolve_nostr_uri",
			mcp.WithDescription("Resolve URIs prefixed with nostr:, including nostr:nevent1..., nostr:npub1..., nostr:nprofile1... and nostr:naddr1..."),
			mcp.WithString("uri", mcp.Description("URI to be resolved"), mcp.Required()),
		), func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			uri := required[string](r, "uri")
			if strings.HasPrefix(uri, "nostr:") {
				uri = uri[6:]
			}

			prefix, data, err := nip19.Decode(uri)
			if err != nil {
				return mcp.NewToolResultError("this Nostr uri is invalid"), nil
			}

			switch prefix {
			case "npub":
				pm := sys.FetchProfileMetadata(ctx, data.(string))
				return mcp.NewToolResultText(
					fmt.Sprintf("this is a Nostr profile named '%s', their public key is '%s'",
						pm.ShortName(), pm.PubKey),
				), nil
			case "nprofile":
				pm, _ := sys.FetchProfileFromInput(ctx, uri)
				return mcp.NewToolResultText(
					fmt.Sprintf("this is a Nostr profile named '%s', their public key is '%s'",
						pm.ShortName(), pm.PubKey),
				), nil
			case "nevent":
				event, _, err := sys.FetchSpecificEventFromInput(ctx, uri, sdk.FetchSpecificEventParameters{
					WithRelays: false,
				})
				if err != nil {
					return mcp.NewToolResultError("Couldn't find this event anywhere"), nil
				}

				return mcp.NewToolResultText(
					fmt.Sprintf("this is a Nostr event: %s", event),
				), nil
			case "naddr":
				return mcp.NewToolResultError("For now we can't handle this kind of Nostr uri"), nil
			default:
				return mcp.NewToolResultError("We don't know how to handle this Nostr uri"), nil
			}
		})

		s.AddTool(mcp.NewTool("search_profile",
			mcp.WithDescription("Search for the public key of a Nostr user given their name"),
			mcp.WithString("name", mcp.Description("Name to be searched"), mcp.Required()),
		), func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name := required[string](r, "name")
			re := sys.Pool.QuerySingle(ctx, []string{"relay.nostr.band", "nostr.wine"}, nostr.Filter{Search: name, Kinds: []int{0}})
			if re == nil {
				return mcp.NewToolResultError("couldn't find anyone with that name"), nil
			}

			return mcp.NewToolResultText(re.PubKey), nil
		})

		s.AddTool(mcp.NewTool("get_outbox_relay_for_pubkey",
			mcp.WithDescription("Get the best relay from where to read notes from a specific Nostr user"),
			mcp.WithString("pubkey", mcp.Description("Public key of Nostr user we want to know the relay from where to read"), mcp.Required()),
		), func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			pubkey := required[string](r, "pubkey")
			res := sys.FetchOutboxRelays(ctx, pubkey, 1)
			return mcp.NewToolResultText(res[0]), nil
		})

		s.AddTool(mcp.NewTool("read_events_from_relay",
			mcp.WithDescription("Makes a REQ query to one relay using the specified parameters, this can be used to fetch notes from a profile"),
			mcp.WithString("relay", mcp.Description("relay URL to send the query to"), mcp.Required()),
			mcp.WithNumber("kind", mcp.Description("event kind number to include in the 'kinds' field"), mcp.Required()),
			mcp.WithNumber("limit", mcp.Description("maximum number of events to query"), mcp.Required()),
			mcp.WithString("pubkey", mcp.Description("pubkey to include in the 'authors' field")),
		), func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			relay := required[string](r, "relay")
			kind := int(required[float64](r, "kind"))
			limit := int(required[float64](r, "limit"))
			pubkey, _ := optional[string](r, "pubkey")

			if pubkey != "" && !nostr.IsValidPublicKey(pubkey) {
				return mcp.NewToolResultError("the given pubkey isn't a valid public key, it must be 32 bytes hex, like the ones returned by search_profile"), nil
			}

			filter := nostr.Filter{
				Limit: limit,
				Kinds: []int{kind},
			}
			if pubkey != "" {
				filter.Authors = []string{pubkey}
			}

			events := sys.Pool.FetchMany(ctx, []string{relay}, filter)

			result := strings.Builder{}
			for ie := range events {
				result.WriteString("author public key: ")
				result.WriteString(ie.PubKey)
				result.WriteString("content: '")
				result.WriteString(ie.Content)
				result.WriteString("'")
				result.WriteString("\n---\n")
			}

			return mcp.NewToolResultText(result.String()), nil
		})

		return server.ServeStdio(s)
	},
}

func required[T comparable](r mcp.CallToolRequest, p string) T {
	var zero T
	if _, ok := r.Params.Arguments[p]; !ok {
		return zero
	}
	if _, ok := r.Params.Arguments[p].(T); !ok {
		return zero
	}
	if r.Params.Arguments[p].(T) == zero {
		return zero
	}
	return r.Params.Arguments[p].(T)
}

func optional[T any](r mcp.CallToolRequest, p string) (T, bool) {
	var zero T
	if _, ok := r.Params.Arguments[p]; !ok {
		return zero, false
	}
	if _, ok := r.Params.Arguments[p].(T); !ok {
		return zero, false
	}
	return r.Params.Arguments[p].(T), true
}
