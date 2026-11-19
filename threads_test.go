package main

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

func TestThreadScope(t *testing.T) {
	in := threadInput{messageInput: messageInput{UID: 1, UIDValidity: 2}, Folders: []string{"inbox", "Archive", "Archive"}}
	folders, err := in.scope()
	if err != nil || strings.Join(folders, ",") != "INBOX,Archive" {
		t.Fatalf("scope: %v, %v", folders, err)
	}
	for _, bad := range [][]string{{"A", "B", "C", "D"}, {"bad\r\n"}} {
		in.Folders = bad
		if _, err := in.scope(); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestThreadLookup(t *testing.T) {
	u := imapmemserver.NewUser("reader", "test-only")
	for _, folder := range []string{"INBOX", "Archive"} {
		if err := u.Create(folder, nil); err != nil {
			t.Fatal(err)
		}
	}
	appendMsg := func(folder, headers string) messageInput {
		msg := "Subject: Shared subject\r\n" + headers + "\r\n\r\nBody"
		data, err := u.Append(folder, strings.NewReader(msg), &imap.AppendOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return messageInput{Account: "test", Folder: folder, UID: data.UID, UIDValidity: data.UIDValidity}
	}
	root := appendMsg("INBOX", "Message-ID: <root@test>")
	child := appendMsg("Archive", "Message-ID: <child@test>\r\nIn-Reply-To: <root@test>")
	grand := appendMsg("INBOX", "References:\r\n <child@test>")
	appendMsg("Archive", "Message-ID: <ROOT@test>")                // IMAP matches case-insensitively
	appendMsg("Archive", "Message-ID: <other@test> (<root@test>)") // comment, not an identifier
	solo := appendMsg("Archive", "From: no-ids@test")
	oversized := appendMsg("INBOX", "References: "+strings.Repeat("<id@test> ", threadHeaderBytes/8))
	backend := imapmemserver.New()
	backend.AddUser(u)
	srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imap.CapSet{imap.CapIMAP4rev1: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return backend.NewSession(), nil, nil
	}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() { _ = srv.Serve(ln) }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := imapclient.New(&limitedConn{Conn: conn, remaining: maxIMAPBytes, deadline: time.Now().Add(5 * time.Second)}, nil)
	defer c.Close()
	if err := c.Login("reader", "test-only").Wait(); err != nil {
		t.Fatal(err)
	}
	lookup := func(anchor messageInput) (threadResult, error) {
		in := threadInput{messageInput: anchor, Folders: []string{"Archive"}}
		folders, err := in.scope()
		if err != nil {
			t.Fatal(err)
		}
		return lookupThread(c, in, folders)
	}
	out, err := lookup(root)
	if err != nil || out.Truncated || len(out.Messages) != 3 {
		t.Fatalf("thread = %+v, %v", out, err)
	}
	for i, want := range []messageInput{root, child, grand} {
		if out.Messages[i].messageInput != want {
			t.Fatalf("wrong reference: %+v", out.Messages[i])
		}
	}
	for _, ref := range []messageInput{root, child, grand} {
		if _, err := examine(c, ref.Folder, ref.UIDValidity); err != nil {
			t.Fatal(err)
		}
		msgs, err := c.Fetch(imap.UIDSetNum(ref.UID), &imap.FetchOptions{Flags: true}).Collect()
		if err != nil || len(msgs) != 1 || hasSeen(msgs[0].Flags) {
			t.Fatalf("unexpected flags for %v: %v %v", ref, msgs, err)
		}
	}
	if out, err := lookup(solo); err != nil || out.Truncated || len(out.Messages) != 1 {
		t.Fatalf("no IDs: %+v, %v", out, err)
	}
	if out, err := lookup(oversized); err != nil || !out.Truncated || len(out.Messages) != 1 {
		t.Fatalf("oversized: %+v, %v", out, err)
	}
	// Start at a message without its own Message-ID and walk back to the root.
	if out, err := lookup(grand); err != nil || out.Truncated || len(out.Messages) != 3 || out.Messages[2].messageInput != root {
		t.Fatalf("ancestors: %+v, %v", out, err)
	}
	for range threadCandidates + 1 {
		appendMsg("Archive", "In-Reply-To: <root@test>")
	}
	if out, err := lookup(root); err != nil || !out.Truncated || len(out.Messages) != threadCandidates+1 {
		t.Fatalf("candidate cap: got %d, truncated=%v, err=%v", len(out.Messages), out.Truncated, err)
	}
	root.UIDValidity++
	if _, err := lookup(root); err == nil {
		t.Fatal("accepted stale anchor")
	}
}
