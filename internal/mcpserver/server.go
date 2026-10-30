// Package mcpserver exposes the CLI commands as tools on a Model Context
// Protocol (MCP) server, so MCP clients such as Claude can read and send
// WhatsApp messages.
//
// Each tool calls the matching commands.App method and returns its JSON
// output unchanged as the tool's text content. A response with
// "success": false is reported as a tool error.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vicentereig/whatsapp-cli/internal/output"
)

// Backend is the subset of commands.App that the MCP tools call.
// Defined here (at consumer) so tests can inject a fake.
type Backend interface {
	ListChats(query *string, limit, page int) string
	SearchContacts(query string) string
	ListMessages(chatJID *string, query *string, since *string, limit, page int) string
	SendMessage(ctx context.Context, recipient, message string) string
	SendImage(ctx context.Context, recipient, imagePath, caption string) string
	DownloadMedia(ctx context.Context, messageID string, chatJID *string, outputPath string) string
}

const defaultLimit = 20

type listChatsInput struct {
	Query string `json:"query,omitempty" jsonschema:"only return chats whose name or JID contains this text"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of chats to return (default 20)"`
	Page  int    `json:"page,omitempty" jsonschema:"page number for pagination, starting at 0"`
}

type searchContactsInput struct {
	Query string `json:"query" jsonschema:"text to match against contact names and phone numbers"`
}

type listMessagesInput struct {
	Chat  string `json:"chat,omitempty" jsonschema:"only return messages from this chat JID; omit for all chats"`
	Since string `json:"since,omitempty" jsonschema:"only return messages at or after this time (inclusive): RFC 3339 like 2026-10-30T15:04:05Z, or a date like 2026-10-30 (local midnight)"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of messages to return (default 20)"`
	Page  int    `json:"page,omitempty" jsonschema:"page number for pagination, starting at 0"`
}

type searchMessagesInput struct {
	Query string `json:"query" jsonschema:"text to search for in message content"`
	Chat  string `json:"chat,omitempty" jsonschema:"only search this chat JID; omit to search all chats"`
	Since string `json:"since,omitempty" jsonschema:"only return messages at or after this time (inclusive): RFC 3339 like 2026-10-30T15:04:05Z, or a date like 2026-10-30 (local midnight)"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of messages to return (default 20)"`
	Page  int    `json:"page,omitempty" jsonschema:"page number for pagination, starting at 0"`
}

type sendMessageInput struct {
	To      string `json:"to" jsonschema:"recipient: phone number with country code and no '+' (e.g. 15551234567), or a full JID (e.g. 15551234567@s.whatsapp.net, or 1203630000@g.us for a group)"`
	Message string `json:"message" jsonschema:"text of the message"`
}

type sendImageInput struct {
	To        string `json:"to" jsonschema:"recipient: phone number with country code and no '+' (e.g. 15551234567), or a full JID (e.g. 15551234567@s.whatsapp.net, or 1203630000@g.us for a group)"`
	ImagePath string `json:"image_path" jsonschema:"path to the image file on the machine running the server"`
	Caption   string `json:"caption,omitempty" jsonschema:"optional caption shown under the image"`
}

type downloadMediaInput struct {
	MessageID string `json:"message_id" jsonschema:"ID of the message that holds the media"`
	Chat      string `json:"chat,omitempty" jsonschema:"chat JID of the message; needed only if the ID is not unique"`
	Output    string `json:"output,omitempty" jsonschema:"file or directory to save to; omit to save under the store's media directory"`
}

// New builds an MCP server whose tools call backend.
func New(backend Backend, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "whatsapp-cli", Version: version}, nil)

	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_chats",
		Description: "List WhatsApp chats from the local database, most recent first.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listChatsInput) (*mcp.CallToolResult, any, error) {
		return toolResult(backend.ListChats(optionalStr(in.Query), limitOrDefault(in.Limit), pageOrZero(in.Page))), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_contacts",
		Description: "Search WhatsApp contacts by name or phone number.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in searchContactsInput) (*mcp.CallToolResult, any, error) {
		if err := requireField("query", in.Query); err != nil {
			return toolResult(output.Error(err)), nil, nil
		}
		return toolResult(backend.SearchContacts(in.Query)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_messages",
		Description: "List WhatsApp messages from the local database, newest first, optionally for one chat. " +
			"To check for new messages, pass the timestamp of the newest message already seen as since; " +
			"since is inclusive, so skip message IDs you have already seen.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listMessagesInput) (*mcp.CallToolResult, any, error) {
		return toolResult(backend.ListMessages(optionalStr(in.Chat), nil, optionalStr(in.Since), limitOrDefault(in.Limit), pageOrZero(in.Page))), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_messages",
		Description: "Search the text of WhatsApp messages in the local database.",
		Annotations: readOnly,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in searchMessagesInput) (*mcp.CallToolResult, any, error) {
		if err := requireField("query", in.Query); err != nil {
			return toolResult(output.Error(err)), nil, nil
		}
		return toolResult(backend.ListMessages(optionalStr(in.Chat), &in.Query, optionalStr(in.Since), limitOrDefault(in.Limit), pageOrZero(in.Page))), nil, nil
	})

	// Sending reaches real people and cannot be undone, so these tools are
	// left with the spec's default destructiveHint (true).
	send := &mcp.ToolAnnotations{OpenWorldHint: boolPtr(true)}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_message",
		Description: "Send a WhatsApp text message. This delivers a real message and cannot be undone.",
		Annotations: send,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in sendMessageInput) (*mcp.CallToolResult, any, error) {
		if err := requireField("to", in.To); err != nil {
			return toolResult(output.Error(err)), nil, nil
		}
		if err := requireField("message", in.Message); err != nil {
			return toolResult(output.Error(err)), nil, nil
		}
		return toolResult(backend.SendMessage(ctx, in.To, in.Message)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_image",
		Description: "Send an image file as a WhatsApp message, with an optional caption. This delivers a real message and cannot be undone.",
		Annotations: send,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in sendImageInput) (*mcp.CallToolResult, any, error) {
		if err := requireField("to", in.To); err != nil {
			return toolResult(output.Error(err)), nil, nil
		}
		if err := requireField("image_path", in.ImagePath); err != nil {
			return toolResult(output.Error(err)), nil, nil
		}
		return toolResult(backend.SendImage(ctx, in.To, in.ImagePath, in.Caption)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "download_media",
		Description: "Download the image, video, audio, or document attached to a message and return the saved file path.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(true)},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in downloadMediaInput) (*mcp.CallToolResult, any, error) {
		if err := requireField("message_id", in.MessageID); err != nil {
			return toolResult(output.Error(err)), nil, nil
		}
		return toolResult(backend.DownloadMedia(ctx, in.MessageID, optionalStr(in.Chat), in.Output)), nil, nil
	})

	return s
}

// Run serves MCP over newline-delimited JSON on in and out until ctx is
// done or the client disconnects.
func Run(ctx context.Context, backend Backend, version string, in io.ReadCloser, out io.WriteCloser) error {
	return New(backend, version).Run(ctx, &mcp.IOTransport{Reader: in, Writer: out})
}

// toolResult wraps a command's JSON output as a tool result, marking it as
// an error when the command reported "success": false.
func toolResult(jsonOut string) *mcp.CallToolResult {
	var r struct {
		Success bool `json:"success"`
	}
	_ = json.Unmarshal([]byte(jsonOut), &r)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: jsonOut}},
		IsError: !r.Success,
	}
}

func requireField(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func optionalStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func limitOrDefault(limit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	return limit
}

func pageOrZero(page int) int {
	if page < 0 {
		return 0
	}
	return page
}

func boolPtr(b bool) *bool {
	return &b
}
