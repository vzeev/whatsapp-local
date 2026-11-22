package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const stateDir = ".whatsapp"

var groupID = regexp.MustCompile(`^[0-9]+(-[0-9]+)?@g\.us$`)

type message struct {
	Group      string    `json:"group_id"`
	ID         string    `json:"message_id"`
	At         time.Time `json:"sent_at"`
	Sender     string    `json:"sender"`
	Text       string    `json:"text"`
	Attachment bool      `json:"attachment,omitempty"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "wa:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("use ./wa --help")
	}
	if args[0] == "demo" && len(args) == 1 {
		return demo(time.Now(), os.Stdout)
	}
	messagesPath := filepath.Join(stateDir, "messages.sqlite3")
	switch args[0] {
	case "read":
		return readCommand(messagesPath, args[1:])
	case "watch":
		if len(args) < 2 {
			return errors.New("usage: ./wa watch GROUP_ID [GROUP_ID ...]")
		}
		for _, id := range args[1:] {
			if !groupID.MatchString(id) {
				return errors.New("use a group ID ending in @g.us from ./wa groups")
			}
		}
	case "pair", "groups":
		if len(args) != 1 {
			return fmt.Errorf("usage: ./wa %s", args[0])
		}
	default:
		return errors.New("unknown command. Run ./wa --help")
	}

	// A linked-device session can only be used by one network command at a time.
	lock, err := os.OpenFile(filepath.Join(stateDir, "session.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("the session is busy. Stop the other pair, groups, or watch command")
	}

	sessionPath := filepath.Join(stateDir, "session.sqlite3")
	container, err := sqlstore.New(ctx, "sqlite3", sqliteURI(sessionPath, "rwc"), nil)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer container.Close()
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	client := whatsmeow.NewClient(device, nil)
	client.BackgroundEventCtx = ctx
	if args[0] == "pair" {
		if device.ID != nil {
			fmt.Fprintln(os.Stderr, "Already linked. Run ./wa groups.")
			return nil
		}
		return pair(ctx, client)
	}
	if device.ID == nil {
		return errors.New("run ./wa pair first")
	}
	if args[0] == "groups" {
		return groups(ctx, client)
	}
	return watch(ctx, client, messagesPath, args[1:])
}

func pair(ctx context.Context, client *whatsmeow.Client) error {
	defer client.Disconnect()
	store.DeviceProps.RequireFullSync = proto.Bool(false)
	store.DeviceProps.HistorySyncConfig = nil
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	qr, err := client.GetQRChannel(ctx)
	if err != nil {
		return errors.New("could not start QR pairing")
	}
	if err := client.ConnectContext(ctx); err != nil {
		return errors.New("could not connect to WhatsApp for pairing")
	}
	fmt.Fprintln(os.Stderr, "On your phone, open WhatsApp > Linked devices > Link a device.")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item, ok := <-qr:
			if !ok {
				return errors.New("pairing ended. Run ./wa pair for a fresh code")
			}
			switch item.Event {
			case whatsmeow.QRChannelEventCode:
				qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stderr)
			case whatsmeow.QRChannelSuccess.Event:
				fmt.Fprintln(os.Stderr, "Linked. Run ./wa groups.")
				return nil
			default:
				return fmt.Errorf("pairing ended (%s). Run ./wa pair for a fresh code", item.Event)
			}
		}
	}
}

func groups(ctx context.Context, client *whatsmeow.Client) error {
	defer client.Disconnect()
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if err := client.ConnectContext(ctx); err != nil || !client.WaitForConnection(10*time.Second) {
		return errors.New("could not sign in to WhatsApp. Check the linked device on your phone")
	}
	joined, err := client.GetJoinedGroups(ctx)
	if err != nil {
		return errors.New("could not list WhatsApp groups")
	}
	enc := json.NewEncoder(os.Stdout)
	for _, group := range joined {
		if group == nil || group.IsParent || !groupID.MatchString(group.JID.String()) {
			continue
		}
		row := struct {
			ID           string `json:"group_id"`
			Name         string `json:"name"`
			Announcement bool   `json:"community_announcement,omitempty"`
		}{group.JID.String(), group.Name, group.IsDefaultSubGroup}
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

func watch(ctx context.Context, client *whatsmeow.Client, path string, ids []string) error {
	db, err := openMessages(sqliteURI(path, "rwc"))
	if err != nil {
		return err
	}
	defer db.Close()
	starts, err := trackGroups(db, ids, time.Now().Unix())
	if err != nil {
		return err
	}

	failure := make(chan error, 1)
	fail := func(err error) {
		select {
		case failure <- err:
		default:
		}
	}
	client.SynchronousAck = true
	handler := client.AddEventHandlerWithSuccessStatus(func(event any) bool {
		switch value := event.(type) {
		case *events.Connected:
			fmt.Fprintf(os.Stderr, "Connected. Watching %d group(s). Press Ctrl+C to stop.\n", len(starts))
		case *events.Disconnected:
			fmt.Fprintln(os.Stderr, "Disconnected. Waiting for WhatsApp to reconnect.")
		case *events.LoggedOut:
			fail(errors.New("WhatsApp unlinked this session. Run ./wa pair again"))
		case *events.StreamReplaced, *events.ClientOutdated, *events.TemporaryBan:
			fail(errors.New("WhatsApp ended the session. Check Linked devices on your phone"))
		case *events.ConnectFailure:
			if value.Reason < 500 {
				fail(errors.New("WhatsApp refused this session. Check Linked devices on your phone"))
			}
		case *events.Message:
			if m, ok := collectMessage(value, starts); ok {
				if err := saveMessage(db, m); err != nil {
					fail(fmt.Errorf("save message: %w", err))
					return false
				}
			}
		}
		return true
	})
	defer client.RemoveEventHandler(handler)
	defer client.Disconnect()
	if err := client.ConnectContext(ctx); err != nil {
		return errors.New("could not connect to WhatsApp. Try ./wa watch again")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-failure:
		return err
	}
}

func collectMessage(event *events.Message, starts map[string]int64) (message, bool) {
	if event == nil || event.Message == nil || event.IsEphemeral || event.IsViewOnce || event.IsEdit {
		return message{}, false
	}
	info := event.Info
	id := info.Chat.String()
	start, tracked := starts[id]
	if !tracked || info.ID == "" || info.Timestamp.IsZero() || info.Timestamp.Unix() < start {
		return message{}, false
	}
	sender := info.PushName
	if info.IsFromMe {
		sender = "You"
	} else if sender == "" {
		sender = "Unknown sender"
	}
	m := message{Group: id, ID: info.ID, At: info.Timestamp.UTC(), Sender: sender}
	body := event.Message
	switch {
	case body.GetImageMessage() != nil:
		m.Text, m.Attachment = body.GetImageMessage().GetCaption(), true
	case body.GetVideoMessage() != nil:
		m.Text, m.Attachment = body.GetVideoMessage().GetCaption(), true
	case body.GetDocumentMessage() != nil:
		m.Text, m.Attachment = body.GetDocumentMessage().GetCaption(), true
	case body.GetAudioMessage() != nil || body.GetStickerMessage() != nil:
		m.Attachment = true
	default:
		m.Text = body.GetConversation()
		if m.Text == "" {
			m.Text = body.GetExtendedTextMessage().GetText()
		}
	}
	return m, m.Text != "" || m.Attachment
}

func sqliteURI(path, mode string) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?mode=" + mode + "&_busy_timeout=5000&_foreign_keys=on"
}

func openMessages(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL;
		CREATE TABLE IF NOT EXISTS tracked_groups (id TEXT PRIMARY KEY, starts_at INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS messages (
			group_id TEXT NOT NULL, message_id TEXT NOT NULL, sent_at INTEGER NOT NULL,
			sender TEXT NOT NULL, text TEXT NOT NULL, attachment INTEGER NOT NULL,
			PRIMARY KEY (group_id, message_id)
		);
		CREATE INDEX IF NOT EXISTS messages_by_time ON messages(group_id, sent_at, message_id);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func trackGroups(db *sql.DB, ids []string, now int64) (map[string]int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	starts := make(map[string]int64, len(ids))
	for _, id := range ids {
		if _, err := tx.Exec(`INSERT INTO tracked_groups VALUES (?, ?) ON CONFLICT(id) DO NOTHING`, id, now); err != nil {
			return nil, err
		}
		var start int64
		if err := tx.QueryRow(`SELECT starts_at FROM tracked_groups WHERE id = ?`, id).Scan(&start); err != nil {
			return nil, err
		}
		starts[id] = start
	}
	return starts, tx.Commit()
}

func saveMessage(db *sql.DB, m message) error {
	_, err := db.Exec(`INSERT INTO messages VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(group_id, message_id) DO NOTHING`, m.Group, m.ID, m.At.Unix(), m.Sender, m.Text, m.Attachment)
	return err
}

func readCommand(path string, args []string) error {
	if len(args) == 0 || !groupID.MatchString(args[0]) {
		return errors.New("usage: ./wa read GROUP_ID [-since 24h] [-limit 200]")
	}
	flags := flag.NewFlagSet("read", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	since := flags.Duration("since", 24*time.Hour, "how far back to look")
	limit := flags.Int("limit", 200, "maximum number of messages")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *since <= 0 || *limit <= 0 || *limit > 10000 {
		return errors.New("use a positive duration such as 24h and a limit from 1 to 10000")
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return errors.New("no local message database yet. Run ./wa watch GROUP_ID first")
	} else if err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", sqliteURI(path, "ro"))
	if err != nil {
		return err
	}
	defer db.Close()
	var start int64
	if err := db.QueryRow(`SELECT starts_at FROM tracked_groups WHERE id = ?`, args[0]).Scan(&start); errors.Is(err, sql.ErrNoRows) {
		return errors.New("this group has no local collection. Use its ID with ./wa watch")
	} else if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Tracking started at", time.Unix(start, 0).UTC().Format(time.RFC3339))
	return printMessages(db, os.Stdout, args[0], time.Now().Add(-*since), *limit)
}

func printMessages(db *sql.DB, out io.Writer, id string, since time.Time, limit int) error {
	rows, err := db.Query(`SELECT group_id, message_id, sent_at, sender, text, attachment
		FROM messages WHERE group_id = ? AND sent_at >= ? ORDER BY sent_at DESC, message_id DESC LIMIT ?`,
		id, since.Unix(), limit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	var messages []message
	for rows.Next() {
		var m message
		var at int64
		if err := rows.Scan(&m.Group, &m.ID, &at, &m.Sender, &m.Text, &m.Attachment); err != nil {
			return err
		}
		m.At = time.Unix(at, 0).UTC()
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(messages) > limit {
		fmt.Fprintf(os.Stderr, "More messages match this window. Showing the latest %d.\n", limit)
		messages = messages[:limit]
	}
	if len(messages) == 0 {
		fmt.Fprintln(os.Stderr, "No collected messages in the requested window.")
	}
	slices.Reverse(messages)
	enc := json.NewEncoder(out)
	for _, m := range messages {
		if err := enc.Encode(m); err != nil {
			return err
		}
	}
	return nil
}

func demo(now time.Time, out io.Writer) error {
	db, err := openMessages(":memory:")
	if err != nil {
		return err
	}
	defer db.Close()
	const group = "120000000001@g.us"
	for _, m := range []message{
		{group, "demo-1", now.Add(-30 * time.Minute), "Maya", "Room B is booked for the demo at 15:00.", false},
		{group, "demo-2", now.Add(-20 * time.Minute), "Sam", "I will bring the projector adapter.", false},
		{group, "demo-3", now.Add(-10 * time.Minute), "Noa", "Draft schedule attached. Can someone confirm the Q&A slot?", true},
	} {
		if err := saveMessage(db, m); err != nil {
			return err
		}
	}
	return printMessages(db, out, group, now.Add(-time.Hour), 20)
}
