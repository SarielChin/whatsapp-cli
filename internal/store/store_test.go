package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDB(t *testing.T) *MessageStore {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := NewMessageStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	return store
}

func TestNewMessageStore(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := NewMessageStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	// Verify database file was created
	_, err = os.Stat(dbPath)
	assert.NoError(t, err)
}

// messages.db holds the full plaintext archive — message bodies, contact names
// and phone numbers. The directory holding it must not be readable by other
// local users.
func TestNewMessageStoreCreatesPrivateDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	storeDir := filepath.Join(tmpDir, "store")
	dbPath := filepath.Join(storeDir, "messages.db")

	store, err := NewMessageStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	info, err := os.Stat(storeDir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0700), info.Mode().Perm(),
		"store directory must be owner-only; it holds the plaintext message archive")
}

func TestStoreChat(t *testing.T) {
	store := setupTestDB(t)

	err := store.StoreChat("1234@s.whatsapp.net", "John Doe", time.Now())
	assert.NoError(t, err)
}

func TestStoreChatDoesNotOverwriteFriendlyWithJID(t *testing.T) {
	store := setupTestDB(t)
	jid := "1234@s.whatsapp.net"

	require.NoError(t, store.StoreChat(jid, "John Doe", time.Now()))
	require.NoError(t, store.StoreChat(jid, jid, time.Now().Add(time.Minute)))

	chats, err := store.ListChats(ListChatsParams{Limit: 1})
	require.NoError(t, err)
	require.NotEmpty(t, chats)
	assert.Equal(t, "John Doe", chats[0].Name)
}

func TestStoreChatUpgradesNameFromJID(t *testing.T) {
	store := setupTestDB(t)
	jid := "5678@s.whatsapp.net"

	require.NoError(t, store.StoreChat(jid, jid, time.Now()))
	require.NoError(t, store.StoreChat(jid, "Jane Smith", time.Now().Add(time.Minute)))

	chats, err := store.ListChats(ListChatsParams{Limit: 1})
	require.NoError(t, err)
	require.NotEmpty(t, chats)
	assert.Equal(t, "Jane Smith", chats[0].Name)
}

func TestStoreMessage(t *testing.T) {
	store := setupTestDB(t)

	// First store a chat
	chatJID := "1234@s.whatsapp.net"
	err := store.StoreChat(chatJID, "John Doe", time.Now())
	require.NoError(t, err)

	// Then store a message
	err = store.StoreMessage("msg1", chatJID, "1234", "Hello", time.Now(), false, "", "", "", "", "", nil, nil, nil, 0)
	assert.NoError(t, err)
}

func TestListMessages(t *testing.T) {
	store := setupTestDB(t)
	chatJID := "1234@s.whatsapp.net"

	// Setup test data
	store.StoreChat(chatJID, "John Doe", time.Now())
	now := time.Now()
	store.StoreMessage("msg1", chatJID, "1234", "Hello", now, false, "", "", "", "", "", nil, nil, nil, 0)
	store.StoreMessage("msg2", chatJID, "1234", "World", now.Add(time.Second), false, "", "", "", "", "", nil, nil, nil, 0)

	messages, err := store.ListMessages(ListMessagesParams{ChatJID: &chatJID, Limit: 10})
	require.NoError(t, err)
	assert.Len(t, messages, 2)
	assert.Equal(t, "World", messages[0].Content) // Most recent first
	assert.Equal(t, "Hello", messages[1].Content)
}

// TestListMessages_SinceComparesInstantsAcrossZones guards the `since` filter
// used for polling. go-sqlite3 stores times as text with a UTC offset, so a
// plain string comparison would compare wall-clock times: a message at 10:00
// EST (15:00 UTC) would sort before a 14:00 UTC cutoff and be dropped.
func TestListMessages_SinceComparesInstantsAcrossZones(t *testing.T) {
	store := setupTestDB(t)
	chatJID := "1234@s.whatsapp.net"
	est := time.FixedZone("EST", -5*3600)
	edt := time.FixedZone("EDT", -4*3600)

	store.StoreChat(chatJID, "John Doe", time.Now())
	// 13:00 UTC, before the cutoff.
	store.StoreMessage("old", chatJID, "1234", "old", time.Date(2026, 3, 8, 9, 0, 0, 0, edt), false, "", "", "", "", "", nil, nil, nil, 0)
	// 14:00 UTC, exactly at the cutoff: included, so polling from the last
	// seen timestamp never skips a message sent in the same second.
	store.StoreMessage("edge", chatJID, "1234", "edge", time.Date(2026, 3, 8, 9, 0, 0, 0, est), false, "", "", "", "", "", nil, nil, nil, 0)
	// 15:00 UTC, after the cutoff.
	store.StoreMessage("new", chatJID, "1234", "new", time.Date(2026, 3, 8, 10, 0, 0, 0, est), false, "", "", "", "", "", nil, nil, nil, 0)

	since := time.Date(2026, 3, 8, 14, 0, 0, 0, time.UTC)
	messages, err := store.ListMessages(ListMessagesParams{Since: &since, Limit: 10})
	require.NoError(t, err)

	var ids []string
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	assert.Equal(t, []string{"new", "edge"}, ids)
}

// Sent messages are stored with time.Now(), which carries nanoseconds;
// julianday() must still parse them, or `since` would drop every one.
func TestListMessages_SinceHandlesFractionalSeconds(t *testing.T) {
	store := setupTestDB(t)
	chatJID := "1234@s.whatsapp.net"
	now := time.Now()

	store.StoreChat(chatJID, "John Doe", now)
	store.StoreMessage("before", chatJID, "me", "before", now.Add(-time.Hour), true, "", "", "", "", "", nil, nil, nil, 0)
	store.StoreMessage("after", chatJID, "me", "after", now, true, "", "", "", "", "", nil, nil, nil, 0)

	since := now.Add(-time.Minute).UTC()
	messages, err := store.ListMessages(ListMessagesParams{Since: &since, Limit: 10})
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "after", messages[0].ID)
}

func TestGetMessageForDownload(t *testing.T) {
	store := setupTestDB(t)
	chatJID := "1234@s.whatsapp.net"

	require.NoError(t, store.StoreChat(chatJID, "John Doe", time.Now()))

	now := time.Now().UTC().Truncate(time.Second)
	mediaKey := []byte{1, 2, 3}
	fileSHA := []byte{4, 5, 6}
	fileEncSHA := []byte{7, 8, 9}

	err := store.StoreMessage(
		"msg1",
		chatJID,
		"1234",
		"Sample caption",
		now,
		false,
		"image",
		"photo.jpg",
		"https://example.com/image",
		"/media/direct/path",
		"image/jpeg",
		mediaKey,
		fileSHA,
		fileEncSHA,
		1024,
	)
	require.NoError(t, err)

	info, err := store.GetMessageForDownload("msg1", nil)
	require.NoError(t, err)

	assert.Equal(t, "msg1", info.ID)
	assert.Equal(t, chatJID, info.ChatJID)
	assert.Equal(t, "image", info.MediaType)
	assert.Equal(t, "photo.jpg", info.Filename)
	assert.Equal(t, "/media/direct/path", info.DirectPath)
	assert.Equal(t, "image/jpeg", info.MimeType)
	assert.Equal(t, uint64(1024), info.FileLength)
	assert.Equal(t, mediaKey, info.MediaKey)
	assert.Equal(t, fileSHA, info.FileSHA256)
	assert.Equal(t, fileEncSHA, info.FileEncSHA256)
	assert.Nil(t, info.LocalPath)

	err = store.MarkMediaDownloaded("msg1", chatJID, "/tmp/photo.jpg", now.Add(time.Minute))
	require.NoError(t, err)

	infoAfter, err := store.GetMessageForDownload("msg1", nil)
	require.NoError(t, err)

	require.NotNil(t, infoAfter.LocalPath)
	assert.Equal(t, "/tmp/photo.jpg", *infoAfter.LocalPath)
	require.NotNil(t, infoAfter.DownloadedAt)
	assert.True(t, infoAfter.DownloadedAt.Equal(now.Add(time.Minute)))
}

func TestSearchContacts(t *testing.T) {
	store := setupTestDB(t)

	// Setup test data
	store.StoreChat("1234@s.whatsapp.net", "John Doe", time.Now())
	store.StoreChat("5678@s.whatsapp.net", "Jane Smith", time.Now())
	store.StoreChat("9999@g.us", "Group Chat", time.Now()) // Should be excluded

	contacts, err := store.SearchContacts("John")
	require.NoError(t, err)
	assert.Len(t, contacts, 1)
	assert.Equal(t, "John Doe", contacts[0].Name)
}

func TestListChats(t *testing.T) {
	store := setupTestDB(t)

	// Setup test data
	store.StoreChat("1234@s.whatsapp.net", "John Doe", time.Now())
	store.StoreChat("5678@s.whatsapp.net", "Jane Smith", time.Now().Add(-time.Hour))

	chats, err := store.ListChats(ListChatsParams{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, chats, 2)
	assert.Equal(t, "John Doe", chats[0].Name) // Most recent first
}
