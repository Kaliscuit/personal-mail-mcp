package main

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const exampleMessage = `From: Sender <sender@example.net>
To: reader@example.net
Subject: Reservation notice
Date: Tue, 16 Nov 2021 12:00:00 +0000
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="demo"

--demo
Content-Type: text/plain; charset=UTF-8
Content-Transfer-Encoding: quoted-printable

Reservation =E4=BD=A0=E5=A5=BD.
--demo
Content-Type: application/octet-stream
Content-Disposition: attachment; filename=confirmation.bin
Content-Transfer-Encoding: base64

c2Vuc2l0aXZlIGZpbGU=
--demo--
`

type sizedReader struct{ *strings.Reader }

func (r sizedReader) Size() int64 { return r.Reader.Size() }

func TestIMAPReadAndWrite(t *testing.T) {
	backend := imapmemserver.New()
	u := imapmemserver.NewUser("reader", "test-only")
	if err := u.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if err := u.Create("Archive", nil); err != nil {
		t.Fatal(err)
	}
	backend.AddUser(u)
	for range 3 {
		msg := strings.ReplaceAll(exampleMessage, "\n", "\r\n")
		if _, err := u.Append("INBOX", sizedReader{strings.NewReader(msg)}, &imap.AppendOptions{Time: time.Date(2021, 11, 16, 12, 0, 0, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
	}
	srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: imap.CapSet{imap.CapIMAP4rev2: {}}, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return backend.NewSession(), nil, nil
	}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
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
	folders, err := listFolders(c)
	if err != nil || strings.Join(folders.Folders, ",") != "Archive,INBOX" {
		t.Fatalf("folders = %+v, %v", folders, err)
	}
	q := searchInput{From: "sender@example.net", Subject: "reservation", Since: "2021-11-16", Before: "2021-11-17", Limit: 2}
	criteria, err := q.validate()
	if err != nil {
		t.Fatal(err)
	}
	page, err := findMessages(c, q, criteria)
	if err != nil || len(page.Messages) != 2 || page.Messages[0].UID != 3 || page.NextBeforeUID != 2 {
		t.Fatalf("page = %+v, %v", page, err)
	}
	q.BeforeUID, q.UIDValidity = page.NextBeforeUID, page.UIDValidity
	last, err := findMessages(c, q, criteria)
	if err != nil || len(last.Messages) != 1 || last.Messages[0].UID != 1 || last.NextBeforeUID != 0 {
		t.Fatalf("last = %+v, %v", last, err)
	}
	if _, err := messageMetadata(c, messageInput{Folder: "INBOX", UID: 3, UIDValidity: page.UIDValidity + 1}); err == nil {
		t.Fatal("expected stale UID validity to fail")
	}
	meta, err := messageMetadata(c, messageInput{Folder: "INBOX", UID: 3, UIDValidity: page.UIDValidity})
	if err != nil {
		t.Fatal(err)
	}
	files := attachments(meta.BodyStructure)
	if len(files) != 1 || files[0].Filename != "confirmation.bin" || files[0].MediaType != "application/octet-stream" || files[0].Size == 0 {
		t.Fatalf("files = %+v", files)
	}
	body, err := readBody(c, meta)
	if err != nil || strings.TrimSpace(body.Body) != "Reservation 你好." || body.MediaType != "text/plain" {
		t.Fatalf("body = %+v, %v", body, err)
	}
	file, err := downloadAttachment(c, meta, files[0].Part)
	if err != nil || file.ContentBase64 != "c2Vuc2l0aXZlIGZpbGU=" {
		t.Fatalf("attachment: %+v, %v", file, err)
	}
	if _, err := downloadAttachment(c, meta, []int{1}); err == nil {
		t.Fatal("non-attachment part accepted")
	}
	var forward outboundMessage
	if err := prepareForward(c, &forward, meta); err != nil {
		t.Fatal(err)
	}
	if len(forward.attachments) != 1 || forward.attachments[0].name != "original.eml" || string(forward.attachments[0].data) != strings.ReplaceAll(exampleMessage, "\n", "\r\n") {
		t.Fatal("forwarding did not preserve the complete original message")
	}
	flags, err := c.Fetch(imap.UIDSetNum(3), &imap.FetchOptions{Flags: true}).Collect()
	if err != nil || len(flags) != 1 {
		t.Fatalf("flags = %v, %v", flags, err)
	}
	if hasSeen(flags[0].Flags) {
		t.Fatal("reading marked the message seen")
	}
	message := messageInput{Folder: "INBOX", UID: 3, UIDValidity: page.UIDValidity}
	bad := message
	bad.UIDValidity++
	if err := setSeen(c, seenInput{bad, true}); err == nil {
		t.Fatal("stale UID validity accepted for STORE")
	}
	for _, seen := range []bool{true, false} {
		if err := setSeen(c, seenInput{message, seen}); err != nil {
			t.Fatal(err)
		}
		q.BeforeUID = 0
		p, err := findMessages(c, q, criteria)
		if err != nil || len(p.Messages) == 0 || p.Messages[0].Seen != seen {
			t.Fatalf("read status after STORE: %+v, %v", p, err)
		}
	}
	archive := "Archive"
	if _, err := specialFolder(c, imap.MailboxAttrTrash); err == nil {
		t.Fatal("unexpected trash folder")
	}
	if _, err := moveMessage(c, bad, archive); err == nil {
		t.Fatal("stale UID validity accepted for MOVE")
	}
	if moved, err := moveMessage(c, message, archive); err != nil || moved.Destination != archive {
		t.Fatalf("move: %+v, %v", moved, err)
	}
	if _, err := moveMessage(c, message, archive); err == nil {
		t.Fatal("missing message accepted for MOVE")
	}
	q.Folder, q.UIDValidity = archive, 0
	p, err := findMessages(c, q, criteria)
	if err != nil || len(p.Messages) != 1 {
		t.Fatalf("archived messages: %+v, %v", p, err)
	}
}

func TestResponseBudget(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	go func() { _, _ = io.WriteString(peer, "abcdef") }()
	limited := &limitedConn{Conn: client, remaining: 3, deadline: time.Now().Add(time.Second)}
	data, err := io.ReadAll(limited)
	if string(data) != "abc" || err == nil {
		t.Fatalf("got %q, %v", data, err)
	}
}

func TestFetchGuardOpaqueDataAndDepth(t *testing.T) {
	var guard fetchGuard
	literal := strings.Repeat("(", maxFetchDepth+5) + "\r\n)\""
	wire := fmt.Sprintf("* 1 FETCH (BODY[1] {%d}\r\n%s UID 42 ENVELOPE (\"%s\"))\r\n", len(literal), literal, strings.Repeat("(", maxFetchDepth+5))
	for i := range len(wire) {
		if err := guard.inspect([]byte(wire[i : i+1])); err != nil {
			t.Fatal(err)
		}
	}
	if guard.fetch || guard.literalBytes != 0 {
		t.Fatal("guard did not reset after one response")
	}
	if err := guard.inspect([]byte("* 2 FETCH (BODYSTRUCTURE " + strings.Repeat("(", maxFetchDepth))); err != errFetchLimit {
		t.Fatalf("expected depth guard, got %v", err)
	}
}

func TestRelatedBodyUsesDesignatedRoot(t *testing.T) {
	resource := &imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", ID: "resource"}
	root := &imap.BodyStructureSinglePart{Type: "text", Subtype: "html", ID: "root"}
	structure := &imap.BodyStructureMultiPart{Subtype: "related", Children: []imap.BodyStructure{resource, root}, Extended: &imap.BodyStructureMultiPartExt{
		Params: map[string]string{"start": "<root>"},
	}}
	selected, path := selectBody(structure, nil)
	if selected != root || len(path) != 1 || path[0] != 2 {
		t.Fatalf("selected %+v at %v, want HTML root", selected, path)
	}
}

func TestUIDRangeDoesNotExpand(t *testing.T) {
	var set imap.UIDSet
	set.AddRange(1, 4_000_000_000)
	got := recentUIDs(set, 3999, 2)
	if len(got) != 2 || got[0] != 3998 || got[1] != 3997 {
		t.Fatalf("got %v", got)
	}
}
