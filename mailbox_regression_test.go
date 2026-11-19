package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func TestNativeMoveNeverSendsFallback(t *testing.T) {
	in := messageInput{Folder: "INBOX", UID: 9, UIDValidity: 18}
	var err error
	draftFixture(t, "MOVE", func(c *mailboxSession) {
		_, err = moveMessage(c, in, "Archive")
	}, func(p draftPeer) {
		tag, _ := p.expect(`SELECT INBOX`)
		p.write("* 1 EXISTS\r\n* OK [UIDVALIDITY 18] stable\r\n" + tag + " OK selected\r\n")
		tag, _ = p.expect(`UID SEARCH UID 9`)
		p.write("* CAPABILITY IMAP4rev1\r\n* SEARCH 9\r\n" + tag + " OK verified\r\n")
		if line, err := p.r.ReadString('\n'); err != io.EOF {
			t.Fatalf("unexpected command after MOVE withdrawal: %q, %v", line, err)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "native IMAP MOVE") && !strings.Contains(err.Error(), "safe IMAP MOVE") {
		t.Fatalf("expected refusal: %v", err)
	}

	// Exercise the native-only boundary even if the library's own check sees a
	// different capability set than the caller did.
	draftFixture(t, "", func(c *mailboxSession) {
		_, err = c.moveNative(imap.UIDSetNum(9), "Archive")
	}, func(p draftPeer) {
		if line, err := p.r.ReadString('\n'); err != io.EOF {
			t.Fatalf("non-MOVE command reached peer: %q, %v", line, err)
		}
	})
	if err == nil {
		t.Fatal("native-only MOVE must fail without MOVE")
	}
}

func TestFlagsRejectSessionOnlyKeyword(t *testing.T) {
	var err error
	draftFixture(t, "", func(c *mailboxSession) {
		_, err = updateFlags(c.Client, messageInput{Folder: "INBOX", UIDValidity: 18}, []imap.UID{9}, flagUpdate{add: []imap.Flag{"project"}})
	}, func(p draftPeer) {
		tag, _ := p.expect(`SELECT INBOX`)
		p.write("* 1 EXISTS\r\n* OK [UIDVALIDITY 18] stable\r\n* OK [PERMANENTFLAGS (\\Seen)] permanent\r\n" + tag + " OK selected\r\n")
		if line, err := p.r.ReadString('\n'); err != io.EOF {
			t.Fatalf("unexpected command for a nonpersistent keyword: %q, %v", line, err)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "permanent support") {
		t.Fatalf("expected permanent flag validation, got %v", err)
	}
}

func TestRenameProtectsAdditionalFolderRoles(t *testing.T) {
	for _, folder := range []string{"Projects", "Projects/Queued"} {
		t.Run(folder, func(t *testing.T) {
			var err error
			draftFixture(t, "", func(c *mailboxSession) {
				err = changeFolder(c.Client, folder, "Renamed", false, nil)
			}, func(p draftPeer) {
				tag, _ := p.expect(`LIST .*`)
				p.write("* LIST (\\HasChildren) \"/\" \"Projects\"\r\n* LIST (\\Scheduled \\HasNoChildren) \"/\" \"Projects/Queued\"\r\n" + tag + " OK listed\r\n")
				if line, err := p.r.ReadString('\n'); err != io.EOF {
					t.Fatalf("unexpected command for a protected folder: %q, %v", line, err)
				}
			})
			if err == nil || !strings.Contains(err.Error(), "special-use") {
				t.Fatalf("expected special-use protection, got %v", err)
			}
		})
	}
}

func TestMailboxScopeOrderAndOverlap(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	fast := make(chan struct{})
	out := queryScopes(ctx, []mailboxScope{{Account: "slow"}, {Account: "fast"}}, func(ctx context.Context, i int, _ mailboxScope) (int, error) {
		if i == 1 {
			close(fast)
			return 42, nil
		}
		select {
		case <-fast:
			return 0, errors.New("independent slow mailbox failure")
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	})
	if len(out.Scopes) != 2 || out.Scopes[0].Error != "independent slow mailbox failure" || out.Scopes[1].Result == nil || *out.Scopes[1].Result != 42 {
		t.Fatalf("wrong order or non-overlapping scopes: %+v", out.Scopes)
	}
}

func TestSummarySizeLimits(t *testing.T) {
	from := make([]imap.Address, 10)
	for i := range from {
		from[i] = imap.Address{Mailbox: "sender", Host: "example.org"}
	}
	out := summarize(1, &imap.Envelope{From: from, Subject: strings.Repeat("米", 800), MessageID: "sample@example.org"})
	flags := make([]imap.Flag, 20)
	for i := range flags {
		flags[i] = "project"
	}
	setSummaryFlags(&out, flags)
	if !out.SummaryTruncated || len(out.Subject) != 1023 || len(out.From) != 8 || len(out.Keywords) != 16 || out.MessageID != "<sample@example.org>" {
		t.Fatal("summary limits not applied")
	}
}
