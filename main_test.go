package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestCollectMessageScope(t *testing.T) {
	group := types.NewJID("120000000001", types.GroupServer)
	start := time.Unix(1800000000, 0).UTC()
	starts := map[string]int64{group.String(): start.Unix()}
	event := events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: group},
			ID:            "msg-1",
			Timestamp:     start,
			PushName:      "Maya",
		},
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("Room B")}},
	}
	got, ok := collectMessage(&event, starts)
	if !ok || got.Text != "Room B" || !got.Attachment || got.Group != group.String() || got.Sender != "Maya" {
		t.Fatalf("expected collected caption, got %+v, %v", got, ok)
	}
	for _, tc := range []struct {
		name   string
		change func(*events.Message)
	}{
		{"direct chat", func(m *events.Message) { m.Info.Chat = types.NewJID("1234567", types.DefaultUserServer) }},
		{"unselected group", func(m *events.Message) { m.Info.Chat = types.NewJID("120000000002", types.GroupServer) }},
		{"before tracking", func(m *events.Message) { m.Info.Timestamp = start.Add(-time.Second) }},
		{"disappearing", func(m *events.Message) { m.IsEphemeral = true }},
		{"view once", func(m *events.Message) { m.IsViewOnce = true }},
		{"edit", func(m *events.Message) { m.IsEdit = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := event
			tc.change(&m)
			if _, ok := collectMessage(&m, starts); ok {
				t.Fatal("message should be skipped")
			}
		})
	}
}

func TestLocalReadWhileWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.sqlite3")
	db, err := openMessages(sqliteURI(path, "rwc"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const group, other = "120000000001@g.us", "120000000002@g.us"
	start := time.Unix(1800000000, 0).UTC()
	if _, err := trackGroups(db, []string{group}, start.Unix()); err != nil {
		t.Fatal(err)
	}
	starts, err := trackGroups(db, []string{group, other}, start.Unix()+1)
	if err != nil || starts[group] != start.Unix() || starts[other] != start.Unix()+1 {
		t.Fatalf("unexpected tracking start: %v, %v", starts, err)
	}
	for _, m := range []message{
		{Group: group, ID: "new", At: start.Add(time.Minute), Text: "latest"},
		{Group: other, ID: "new", At: start.Add(time.Minute), Text: "another group"},
		{Group: group, ID: "new", At: start.Add(time.Minute), Text: "duplicate"},
		{Group: group, ID: "first", At: start, Text: "first"},
		{Group: group, ID: "old", At: start.Add(-time.Minute), Text: "before window"},
	} {
		if err := saveMessage(db, m); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := sql.Open("sqlite3", sqliteURI(path, "ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var output bytes.Buffer
	if err := printMessages(reader, &output, group, start, 2); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&output)
	for _, want := range []string{"first", "latest"} {
		var got message
		if err := dec.Decode(&got); err != nil || got.Text != want {
			t.Fatalf("expected %q, got %+v, %v", want, got, err)
		}
	}
	var extra message
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("expected no more messages, got %+v, %v", extra, err)
	}
}
