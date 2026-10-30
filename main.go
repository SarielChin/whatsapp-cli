package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vicentereig/whatsapp-cli/internal/commands"
	"github.com/vicentereig/whatsapp-cli/internal/mcpserver"
	"github.com/vicentereig/whatsapp-cli/internal/output"
)

var (
	// version is overridden at build time via -ldflags "-X main.version=X.Y.Z"
	version = "1.3.3"
)

const (
	// defaultTimeout is the maximum duration for non-sync commands
	defaultTimeout = 5 * time.Minute
)

// optionalStr returns nil for empty strings, otherwise a pointer to the string.
// Used to convert flag values to optional parameters.
func optionalStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

const usage = `WhatsApp CLI - Command line interface for WhatsApp

Usage:
  whatsapp-cli <command> [options]

Commands:
  auth                              Authenticate with WhatsApp (scan QR code)
  sync                              Sync messages continuously (run until Ctrl+C)
  messages list [--chat JID] [--since TIME]          List messages
  messages search --query TEXT [--chat JID] [--since TIME]   Search messages
  contacts search --query TEXT      Search contacts
  chats list                        List chats
  send --to RECIPIENT --message TEXT                     Send a text message
  send --to RECIPIENT --image PATH [--caption TEXT]      Send an image
  media download --message-id ID [--chat JID] [--output PATH]   Download media for a message
  mcp                               Run an MCP server on stdio (also syncs in the background)
  version                           Print CLI version information

Global Options:
  --store DIR    Storage directory (default: ./store)

Examples:
  whatsapp-cli auth
  whatsapp-cli sync                    # Keep running to sync messages
  whatsapp-cli messages list --chat 1234567890@s.whatsapp.net --limit 20
  whatsapp-cli messages search --query "meeting"
  whatsapp-cli contacts search --query "John"
  whatsapp-cli send --to 1234567890 --message "Hello"
  whatsapp-cli send --to 1234567890@g.us --message "Hello group"
`

// extractGlobalFlags pulls --store from anywhere in the arg list,
// returning the store directory and remaining args.
func extractGlobalFlags(args []string) (string, []string) {
	storeDir := "./store"
	var remaining []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--store" && i+1 < len(args):
			storeDir = args[i+1]
			i++ // skip value
		case strings.HasPrefix(args[i], "--store="):
			storeDir = strings.TrimPrefix(args[i], "--store=")
		default:
			remaining = append(remaining, args[i])
		}
	}
	return storeDir, remaining
}

// checkStorePresence rejects commands that would otherwise silently create a
// second, empty store.
//
// --store defaults to "./store", resolved against the CURRENT WORKING
// DIRECTORY, and the store/client constructors create it unconditionally. So
// running any command from the wrong directory creates an empty store rather
// than reporting the mistake — and every later command run from there reads
// that empty store and reports zero messages, which is indistinguishable from
// data loss. An `auth` against such a directory is worse still: it leaves real
// session keys somewhere nobody is tracking.
//
// `auth` is exempt because first-time setup legitimately has nothing to point
// at yet.
func checkStorePresence(command, absStoreDir string) error {
	if command == "auth" {
		return nil
	}
	if _, err := os.Stat(absStoreDir); os.IsNotExist(err) {
		return fmt.Errorf("store does not exist: %s (run 'auth' to create it, or pass --store)", absStoreDir)
	}
	return nil
}

func exitJSON(msg string) {
	fmt.Fprintf(os.Stderr, `{"success":false,"data":null,"error":"%s"}`+"\n", msg)
	os.Exit(1)
}

func requireSubcommand(args []string, command string, valid []string) string {
	if len(args) < 2 {
		exitJSON(fmt.Sprintf("%s requires a subcommand: %s", command, strings.Join(valid, ", ")))
	}
	sub := args[1]
	for _, v := range valid {
		if sub == v {
			return sub
		}
	}
	exitJSON(fmt.Sprintf("unknown %s subcommand: %s (valid: %s)", command, sub, strings.Join(valid, ", ")))
	return "" // unreachable
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	// Extract --store from anywhere in args,
	// so "whatsapp-cli contacts search --store /tmp" works.
	storeDir, args := extractGlobalFlags(os.Args[1:])

	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	command := args[0]

	if command == "version" {
		fmt.Printf(`{"success":true,"data":{"version":"%s"},"error":null}
`, version)
		return
	}

	// Create app
	absStoreDir, err := filepath.Abs(storeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, `{"success":false,"data":null,"error":"invalid store path: %v"}`+"\n", err)
		os.Exit(1)
	}

	if err := checkStorePresence(command, absStoreDir); err != nil {
		exitJSON(err.Error())
	}

	// MCP speaks JSON-RPC on stdout, so nothing else may write there.
	// whatsmeow's loggers print to os.Stdout, so point os.Stdout at stderr
	// and hand the real stdout to the MCP transport only.
	var mcpOut *os.File
	if command == "mcp" {
		mcpOut = os.Stdout
		os.Stdout = os.Stderr
	}

	app, err := commands.NewApp(absStoreDir, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, `{"success":false,"data":null,"error":"Failed to initialize: %v"}
`, err)
		os.Exit(1)
	}
	defer app.Close()

	// Use different timeout for long-running commands
	var ctx context.Context
	var cancel context.CancelFunc
	if command == "sync" || command == "mcp" {
		// For sync and mcp, use signal-based cancellation
		ctx, cancel = context.WithCancel(context.Background())
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigChan
			cancel()
		}()
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), defaultTimeout)
	}
	defer cancel()

	var result string

	switch command {
	case "auth":
		result = app.Auth(ctx)

	case "sync":
		result = app.Sync(ctx)

	case "messages":
		subcommand := requireSubcommand(args, "messages", []string{"list", "search"})
		messagesCmd := flag.NewFlagSet("messages", flag.ExitOnError)
		chatJID := messagesCmd.String("chat", "", "chat JID")
		query := messagesCmd.String("query", "", "search query")
		since := messagesCmd.String("since", "", "only messages at or after this time (RFC 3339 or YYYY-MM-DD)")
		limit := messagesCmd.Int("limit", 20, "limit")
		page := messagesCmd.Int("page", 0, "page")
		// Parse from args[2:] to skip subcommand ("list"/"search") —
		// Go's flag parser stops at the first non-flag argument.
		if len(args) > 2 {
			messagesCmd.Parse(args[2:])
		}

		switch subcommand {
		case "search":
			if *query == "" {
				exitJSON("messages search requires --query")
			}
			result = app.ListMessages(optionalStr(*chatJID), query, optionalStr(*since), *limit, *page)
		case "list":
			result = app.ListMessages(optionalStr(*chatJID), nil, optionalStr(*since), *limit, *page)
		}

	case "contacts":
		requireSubcommand(args, "contacts", []string{"search"})
		contactsCmd := flag.NewFlagSet("contacts", flag.ExitOnError)
		query := contactsCmd.String("query", "", "search query")
		// Parse from args[2:] to skip subcommand ("search") —
		// Go's flag parser stops at the first non-flag argument.
		if len(args) > 2 {
			contactsCmd.Parse(args[2:])
		}

		if *query == "" {
			exitJSON("contacts search requires --query")
		}
		result = app.SearchContacts(*query)

	case "chats":
		requireSubcommand(args, "chats", []string{"list"})
		chatsCmd := flag.NewFlagSet("chats", flag.ExitOnError)
		query := chatsCmd.String("query", "", "search query")
		limit := chatsCmd.Int("limit", 20, "limit")
		page := chatsCmd.Int("page", 0, "page")
		// Parse from args[2:] to skip subcommand ("list") —
		// Go's flag parser stops at the first non-flag argument.
		if len(args) > 2 {
			chatsCmd.Parse(args[2:])
		}

		result = app.ListChats(optionalStr(*query), *limit, *page)

	case "send":
		sendCmd := flag.NewFlagSet("send", flag.ExitOnError)
		to := sendCmd.String("to", "", "recipient")
		message := sendCmd.String("message", "", "message text")
		image := sendCmd.String("image", "", "image file path")
		caption := sendCmd.String("caption", "", "image caption")
		sendCmd.Parse(args[1:])

		if *to == "" {
			exitJSON(`--to is required`)
		}
		if *image != "" && *message != "" {
			exitJSON(`--message and --image are mutually exclusive`)
		}
		if *image != "" {
			result = app.SendImage(ctx, *to, *image, *caption)
		} else if *message != "" {
			result = app.SendMessage(ctx, *to, *message)
		} else {
			exitJSON(`--message or --image required`)
		}

	case "media":
		requireSubcommand(args, "media", []string{"download"})
		downCmd := flag.NewFlagSet("media download", flag.ExitOnError)
		messageID := downCmd.String("message-id", "", "message identifier")
		chatJID := downCmd.String("chat", "", "chat JID (optional)")
		outputPath := downCmd.String("output", "", "output file or directory")
		downCmd.Parse(args[2:])

		if *messageID == "" {
			exitJSON("--message-id required")
		}
		result = app.DownloadMedia(ctx, *messageID, optionalStr(*chatJID), *outputPath)

	case "mcp":
		// The QR code can't be shown through an MCP client, so require an
		// existing session instead of starting the auth flow.
		if !app.IsAuthenticated() {
			exitJSON("not authenticated: run 'whatsapp-cli auth' first")
		}

		// One process holds the only WhatsApp connection: sync runs in the
		// background so the tools read fresh messages, and sends reuse the
		// same connection instead of replacing it.
		mcpCtx, stopMCP := context.WithCancel(ctx)
		syncDone := make(chan struct{})
		go func() {
			defer close(syncDone)
			fmt.Fprintln(os.Stderr, app.Sync(mcpCtx))
		}()

		err := mcpserver.Run(mcpCtx, app, version, os.Stdin, mcpOut)
		stopMCP()
		<-syncDone

		// os.Stdout points at stderr here, so this summary stays off the
		// MCP channel.
		if err != nil {
			result = output.Error(fmt.Errorf("mcp server stopped: %w", err))
		} else {
			result = output.Success(map[string]interface{}{"stopped": true})
		}

	default:
		fmt.Fprintf(os.Stderr, `{"success":false,"data":null,"error":"Unknown command: %s"}
`, command)
		os.Exit(1)
	}

	fmt.Println(result)
}
