package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"github.com/vicentereig/whatsapp-cli/internal/output"
)

// fakeBackend implements Backend for testing and records the last call.
type fakeBackend struct {
	calls []string

	chatJID   *string
	query     *string
	since     *string
	limit     int
	page      int
	recipient string
	message   string
	imagePath string
	caption   string
	messageID string
	output    string

	result string
}

func (f *fakeBackend) reply() string {
	if f.result != "" {
		return f.result
	}
	return output.Success([]string{})
}

func (f *fakeBackend) ListChats(query *string, limit, page int) string {
	f.calls = append(f.calls, "ListChats")
	f.query, f.limit, f.page = query, limit, page
	return f.reply()
}

func (f *fakeBackend) SearchContacts(query string) string {
	f.calls = append(f.calls, "SearchContacts")
	f.query = &query
	return f.reply()
}

func (f *fakeBackend) ListMessages(chatJID *string, query *string, since *string, limit, page int) string {
	f.calls = append(f.calls, "ListMessages")
	f.chatJID, f.query, f.since, f.limit, f.page = chatJID, query, since, limit, page
	return f.reply()
}

func (f *fakeBackend) SendMessage(ctx context.Context, recipient, message string) string {
	f.calls = append(f.calls, "SendMessage")
	f.recipient, f.message = recipient, message
	return f.reply()
}

func (f *fakeBackend) SendImage(ctx context.Context, recipient, imagePath, caption string) string {
	f.calls = append(f.calls, "SendImage")
	f.recipient, f.imagePath, f.caption = recipient, imagePath, caption
	return f.reply()
}

func (f *fakeBackend) DownloadMedia(ctx context.Context, messageID string, chatJID *string, outputPath string) string {
	f.calls = append(f.calls, "DownloadMedia")
	f.messageID, f.chatJID, f.output = messageID, chatJID, outputPath
	return f.reply()
}

// connect starts the server over in-memory transports and returns a client
// session talking to it.
func connect(t *testing.T, backend Backend) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()

	ss, err := New(backend, "test").Connect(ctx, serverT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	return res
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "content should be text, got %T", res.Content[0])
	return text.Text
}

func TestListTools_ExposesAllCommandsWithHints(t *testing.T) {
	cs := connect(t, &fakeBackend{})

	res, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)

	tools := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	require.Len(t, tools, 7)

	for _, name := range []string{"list_chats", "search_contacts", "list_messages", "search_messages"} {
		require.Contains(t, tools, name)
		require.True(t, tools[name].Annotations.ReadOnlyHint, "%s should be read-only", name)
	}
	for _, name := range []string{"send_message", "send_image"} {
		require.Contains(t, tools, name)
		require.False(t, tools[name].Annotations.ReadOnlyHint, "%s must not claim to be read-only", name)
		require.Nil(t, tools[name].Annotations.DestructiveHint, "%s should keep the default destructiveHint", name)
	}
	require.Contains(t, tools, "download_media")
}

func TestListChats_AppliesDefaultLimit(t *testing.T) {
	backend := &fakeBackend{}
	cs := connect(t, backend)

	res := callTool(t, cs, "list_chats", map[string]any{})

	require.False(t, res.IsError)
	require.Equal(t, []string{"ListChats"}, backend.calls)
	require.Nil(t, backend.query)
	require.Equal(t, defaultLimit, backend.limit)
	require.Equal(t, 0, backend.page)
}

func TestSearchMessages_PassesQueryAndChat(t *testing.T) {
	backend := &fakeBackend{}
	cs := connect(t, backend)

	res := callTool(t, cs, "search_messages", map[string]any{
		"query": "invoice", "chat": "123@s.whatsapp.net", "limit": 5, "page": 2,
	})

	require.False(t, res.IsError)
	require.NotNil(t, backend.query)
	require.Equal(t, "invoice", *backend.query)
	require.NotNil(t, backend.chatJID)
	require.Equal(t, "123@s.whatsapp.net", *backend.chatJID)
	require.Equal(t, 5, backend.limit)
	require.Equal(t, 2, backend.page)
}

func TestListMessages_PassesSinceForPolling(t *testing.T) {
	backend := &fakeBackend{}
	cs := connect(t, backend)

	res := callTool(t, cs, "list_messages", map[string]any{"since": "2026-10-30T15:04:05Z"})

	require.False(t, res.IsError)
	require.NotNil(t, backend.since)
	require.Equal(t, "2026-10-30T15:04:05Z", *backend.since)
	require.Nil(t, backend.query, "list_messages must not filter by text")
}

func TestListMessages_OmittedSinceIsNil(t *testing.T) {
	backend := &fakeBackend{}
	cs := connect(t, backend)

	callTool(t, cs, "list_messages", map[string]any{})

	require.Nil(t, backend.since)
}

func TestSearchMessages_RejectsBlankQuery(t *testing.T) {
	backend := &fakeBackend{}
	cs := connect(t, backend)

	res := callTool(t, cs, "search_messages", map[string]any{"query": "   "})

	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "query is required")
	require.Empty(t, backend.calls, "backend must not be called with a blank query")
}

func TestSendMessage_ReturnsCommandJSON(t *testing.T) {
	backend := &fakeBackend{result: output.Success(map[string]any{"sent": true, "id": "ABC"})}
	cs := connect(t, backend)

	res := callTool(t, cs, "send_message", map[string]any{"to": "15551234567", "message": "hi"})

	require.False(t, res.IsError)
	require.Equal(t, "15551234567", backend.recipient)
	require.Equal(t, "hi", backend.message)
	require.JSONEq(t, `{"success":true,"data":{"sent":true,"id":"ABC"},"error":null}`, resultText(t, res))
}

func TestSendMessage_RejectsBlankMessage(t *testing.T) {
	backend := &fakeBackend{}
	cs := connect(t, backend)

	res := callTool(t, cs, "send_message", map[string]any{"to": "15551234567", "message": ""})

	require.True(t, res.IsError)
	require.Empty(t, backend.calls, "nothing may be sent when the message is blank")
}

func TestCommandFailure_IsReportedAsToolError(t *testing.T) {
	backend := &fakeBackend{result: `{"success":false,"data":null,"error":"not connected to WhatsApp"}`}
	cs := connect(t, backend)

	res := callTool(t, cs, "send_image", map[string]any{"to": "15551234567", "image_path": "a.jpg"})

	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "not connected to WhatsApp")
}

func TestDownloadMedia_PassesOptionalArgs(t *testing.T) {
	backend := &fakeBackend{}
	cs := connect(t, backend)

	res := callTool(t, cs, "download_media", map[string]any{"message_id": "M1"})

	require.False(t, res.IsError)
	require.Equal(t, "M1", backend.messageID)
	require.Nil(t, backend.chatJID)
	require.Equal(t, "", backend.output)
}
