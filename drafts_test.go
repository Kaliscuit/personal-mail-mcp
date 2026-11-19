package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
)

// Use a scripted IMAP peer so unexpected writes, fallback commands and duplicate
// APPENDs cannot pass as successful operations.
type draftPeer struct {
	net.Conn
	r *bufio.Reader
	t *testing.T
}

func (p draftPeer) write(s string) {
	p.t.Helper()
	if _, err := io.WriteString(p.Conn, s); err != nil {
		p.t.Fatal(err)
	}
}

func (p draftPeer) expect(pattern string) (string, []string) {
	p.t.Helper()
	line, err := p.r.ReadString('\n')
	if err != nil {
		p.t.Fatal(err)
	}
	tag, cmd, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
	m := regexp.MustCompile("^" + pattern + "$").FindStringSubmatch(cmd)
	if m == nil {
		p.t.Fatalf("command %q does not match %q", cmd, pattern)
	}
	return tag, m
}

// The test drives the peer synchronously. The IMAP client runs concurrently and
// closes its connection on completion, so missing or extra commands fail fast.
func draftFixture(t *testing.T, caps string, call func(*imapclient.Client), script func(draftPeer)) {
	t.Helper()
	conn, peer := net.Pipe()
	c := imapclient.New(conn, nil)
	done := make(chan struct{})
	go func() { defer close(done); defer c.Close(); call(c) }()
	defer func() { peer.Close(); <-done }()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	p := draftPeer{peer, bufio.NewReader(peer), t}
	p.write("* PREAUTH [CAPABILITY " + strings.TrimSpace("IMAP4rev1 "+caps) + "] ready\r\n")
	script(p)
}

func TestDraftAppend(t *testing.T) {
	for _, tc := range []struct {
		name, response     string
		wantUID, uncertain bool
	}{{"appenduid", "OK [APPENDUID 18 9] saved", true, false}, {"no UIDPLUS", "OK saved", false, false}, {"lost confirmation", "", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := (saveDraftInput{Bcc: []string{"private@example.org"}, composition: composition{Body: "body"}}).message()
			if err != nil {
				t.Fatal(err)
			}
			var id string
			var out draftResult
			draftFixture(t, "SPECIAL-USE LIST-EXTENDED", func(c *imapclient.Client) { out, err = saveDraft(c, account{FromAddress: "sender@example.org"}, msg) }, func(p draftPeer) {
				tag, _ := p.expect(`LIST .*RETURN \(SPECIAL-USE\)`)
				p.write(`* LIST (\Drafts) "/" "Unsent"` + "\r\n" + tag + " OK listed\r\n")
				tag, m := p.expect(`APPEND "?Unsent"? \(\\Draft\) \{([0-9]+)\}`)
				size, _ := strconv.Atoi(m[1])
				p.write("+ continue\r\n")
				wire := make([]byte, size+2)
				if _, err := io.ReadFull(p.r, wire); err != nil {
					t.Fatal(err)
				}
				if !bytes.HasSuffix(wire, []byte("\r\n")) {
					t.Fatal("missing literal terminator")
				}
				msg, err := mail.ReadMessage(bytes.NewReader(wire[:size]))
				if err != nil {
					t.Fatal(err)
				}
				bcc, err := msg.Header.AddressList("Bcc")
				if err != nil || len(bcc) != 1 || bcc[0].Address != "private@example.org" || msg.Header.Get("To") != "" {
					t.Fatal("incorrect draft addressing")
				}
				id = msg.Header.Get("Message-ID")
				if tc.response != "" {
					p.write(tag + " " + tc.response + "\r\n")
				}
			})
			if tc.uncertain != (err != nil) {
				t.Fatalf("save: %+v %v", out, err)
			}
			if out.MessageID == "" || out.Folder != "Unsent" {
				t.Fatalf("missing draft identity: %+v, %v", out, err)
			}
			if out.MessageID != id {
				t.Fatalf("Message-ID mismatch: %q, %q", out.MessageID, id)
			}
			if tc.wantUID && (out.UID != 9 || out.UIDValidity != 18) || !tc.wantUID && (out.UID != 0 || out.UIDValidity != 0) {
				t.Fatalf("unexpected identity %+v", out)
			}
			if tc.uncertain && !strings.Contains(err.Error(), out.MessageID) {
				t.Fatalf("uncertain save lacks recovery identity: %v", err)
			}
		})
	}
}

func TestDraftListDiscard(t *testing.T) {
	a := account{DraftsFolder: "Unsent", TrashFolder: "Bin"}
	in := messageInput{Folder: "Unsent", UID: 9, UIDValidity: 18}
	for _, move := range []bool{false, true} {
		t.Run(fmt.Sprintf("native-MOVE-%t", move), func(t *testing.T) {
			caps := ""
			if move {
				caps = "MOVE"
			}
			var page draftPage
			var out moveResult
			var listErr, discardErr, wrongFolderErr error
			draftFixture(t, caps, func(c *imapclient.Client) {
				page, listErr = listDrafts(c, a, listDraftsInput{})
				if listErr != nil {
					return
				}
				other := in
				other.Folder = "INBOX"
				_, wrongFolderErr = discardDraft(c, a, other)
				out, discardErr = discardDraft(c, a, in)
			}, func(p draftPeer) {
				selectReply := "* 1 EXISTS\r\n* OK [UIDVALIDITY 18] stable\r\n"
				tag, _ := p.expect(`EXAMINE "?Unsent"?`)
				p.write(selectReply + tag + " OK [READ-ONLY] examined\r\n")
				tag, _ = p.expect(`UID SEARCH ALL`)
				p.write("* SEARCH 9\r\n" + tag + " OK found\r\n")
				tag, _ = p.expect(`UID FETCH 9 \([A-Z ]+\)`)
				p.write("* 1 FETCH (UID 9 FLAGS (\\Draft))\r\n" + tag + " OK fetched\r\n")
				tag, _ = p.expect(`SELECT "?Unsent"?`)
				p.write(selectReply + tag + " OK selected\r\n")
				if move {
					tag, _ = p.expect(`UID SEARCH UID 9`)
					p.write("* SEARCH 9\r\n" + tag + " OK found\r\n")

					tag, _ = p.expect(`UID MOVE 9 "?Bin"?`)
					p.write(tag + " OK moved\r\n")
				} else {
					// Either ordering of the non-mutating source UID check is safe.
					line, err := p.r.ReadString('\n')
					if err == nil {
						tag, cmd, _ := strings.Cut(line, " ")
						if cmd != "UID SEARCH UID 9\r\n" {
							t.Fatalf("unexpected command without MOVE: %s", cmd)
						}
						p.write("* SEARCH 9\r\n" + tag + " OK found\r\n")
						_, err = p.r.ReadString('\n')
					}
					if err != io.EOF {
						t.Fatalf("unexpected fallback after SELECT: %v", err)
					}
				}
			})
			if listErr != nil || page.Folder != "Unsent" || page.UIDValidity != 18 || len(page.Messages) != 1 || page.Messages[0].UID != 9 || page.Messages[0].Seen {
				t.Fatalf("page = %+v, %v", page, listErr)
			}
			if wrongFolderErr == nil {
				t.Fatal("accepted non-draft folder")
			}
			err := discardErr
			if move && (err != nil || out.Destination != "Bin") || !move && (err == nil || !strings.Contains(err.Error(), "safe IMAP MOVE")) {
				t.Fatalf("discard = %+v, %v", out, err)
			}
		})
	}
}

func TestDraftFolderConfig(t *testing.T) {
	for _, name := range []string{"", "Unsent", "inbox", "Trash", "Archive", "bad\nfolder"} {
		a := account{Provider: "gmail", Username: "me@example.org", PasswordFile: "secret", ArchiveFolder: "Archive", TrashFolder: "Trash", DraftsFolder: name}
		data, err := json.Marshal(config{Interfaces: []string{"lo"}, Port: 8080, TokenFile: "token", Accounts: map[string]account{"me": a}})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig(path)
		valid := name == "" || name == "Unsent"
		if valid != (err == nil) || valid && cfg.Accounts["me"].DraftsFolder != name {
			t.Errorf("drafts_folder %q: %v", name, err)
		}
	}
}
