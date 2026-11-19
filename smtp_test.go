package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	mimemail "github.com/emersion/go-message/mail"
)

func TestSMTPSubmission(t *testing.T) {
	certServer := httptest.NewTLSServer(nil)
	cert := certServer.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(certServer.Certificate())
	name := certServer.Certificate().DNSNames[0]
	certServer.Close()
	for _, tc := range []struct {
		name                                     string
		startTLS                                 bool
		mechanism, credentials, final, wantError string
	}{
		{"implicit/plain", false, "PLAIN", "\x00sender@example.test\x00test-secret", "250 queued\r\n", ""},
		{"starttls/oauth", true, "XOAUTH2", "user=sender@example.test\x01auth=Bearer test-secret\x01\x01", "250 queued\r\n", ""},
		{"rejected", false, "PLAIN", "\x00sender@example.test\x00test-secret", "550 rejected\r\n", "rejected"},
		{"unknown", true, "XOAUTH2", "user=sender@example.test\x01auth=Bearer test-secret\x01\x01", "", "outcome is unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			const username = "sender@example.test"
			var msg outboundMessage
			if err := (recipients{To: []string{"收件人 <to@example.test>"}, Cc: []string{"cc@example.test"}, Bcc: []string{"hidden@example.test"}}).parse(&msg); err != nil {
				t.Fatal(err)
			}
			if err := (composition{Body: "你好\n.leading dot", Attachments: []outgoingAttachment{{Filename: "文件.bin", ContentBase64: "AAH/"}}}).parse(&msg); err != nil {
				t.Fatal(err)
			}
			msg.subject = "测试"
			wire, id, err := encodeMessage(username, msg)
			if err != nil || !strings.HasSuffix(id, "@example.test") {
				t.Fatalf("encode: %q %v", id, err)
			}
			var data []byte
			server := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					server <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				server <- serveSMTPFixture(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, tc.startTLS, tc.mechanism, tc.credentials, tc.final, &data)
			}()
			c, err := openSMTP(ctx, ln.Addr().String(), tc.startTLS, &tls.Config{ServerName: name, RootCAs: roots, MinVersion: tls.VersionTLS12})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			a := account{Username: username}
			if tc.mechanism == "XOAUTH2" {
				a.Provider = "outlook"
			}
			if err := authenticateSMTP(c.Client, a, name, "test-secret"); err != nil {
				t.Fatal(err)
			}
			err = submitSMTP(c.Client, username, msg, wire)
			if (tc.wantError == "" && err != nil) || (tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError))) {
				t.Fatalf("submit = %v, want %q", err, tc.wantError)
			}
			if err := <-server; err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("hidden@example.test")) {
				t.Fatal("BCC leaked into message data")
			}
			m, err := mimemail.CreateReader(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if subject, err := m.Header.Subject(); err != nil || subject != "测试" {
				t.Fatalf("subject: %q %v", subject, err)
			}
			text, err := m.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			if b, err := io.ReadAll(text.Body); err != nil || strings.TrimSpace(string(b)) != "你好\n.leading dot" {
				t.Fatalf("text: %q %v", b, err)
			}
			file, err := m.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			if name, err := file.Header.(*mimemail.AttachmentHeader).Filename(); err != nil || name != "文件.bin" {
				t.Fatalf("filename: %q %v", name, err)
			}
			if b, err := io.ReadAll(file.Body); err != nil || !bytes.Equal(b, []byte{0, 1, 255}) {
				t.Fatalf("attachment: %x %v", b, err)
			}
		})
	}
}

func serveSMTPFixture(conn net.Conn, tlsConfig *tls.Config, startTLS bool, mechanism, credentials, final string, data *[]byte) error {
	upgrade := func() error {
		secured := tls.Server(conn, tlsConfig)
		if err := secured.Handshake(); err != nil {
			return err
		}
		conn = secured
		return nil
	}
	if !startTLS {
		if err := upgrade(); err != nil {
			return err
		}
	}
	reader := textproto.NewReader(bufio.NewReader(conn))
	reply := func(line string) error { _, err := io.WriteString(conn, line); return err }
	expect := func(prefix string) error {
		line, err := reader.ReadLine()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(line, prefix) {
			return fmt.Errorf("expected SMTP command %q", strings.Fields(prefix)[0])
		}
		return nil
	}
	if err := reply("220 fixture\r\n"); err != nil {
		return err
	}
	if startTLS {
		if err := expect("EHLO "); err != nil {
			return err
		}
		if err := reply("250-fixture\r\n250 STARTTLS\r\n"); err != nil {
			return err
		}
		if err := expect("STARTTLS"); err != nil {
			return err
		}
		if err := reply("220 go ahead\r\n"); err != nil {
			return err
		}
		if err := upgrade(); err != nil {
			return err
		}
		reader = textproto.NewReader(bufio.NewReader(conn))
	}
	for _, step := range [][2]string{
		{"EHLO ", "250-fixture\r\n250 AUTH " + mechanism + "\r\n"},
		{"AUTH " + mechanism + " " + base64.StdEncoding.EncodeToString([]byte(credentials)), "235 authenticated\r\n"},
		{"MAIL FROM:<sender@example.test>", "250 sender ok\r\n"},
		{"RCPT TO:<to@example.test>", "250 recipient ok\r\n"},
		{"RCPT TO:<cc@example.test>", "250 recipient ok\r\n"},
		{"RCPT TO:<hidden@example.test>", "250 recipient ok\r\n"},
		{"DATA", "354 send message\r\n"},
	} {
		if err := expect(step[0]); err != nil {
			return err
		}
		if err := reply(step[1]); err != nil {
			return err
		}
	}
	var err error
	*data, err = reader.ReadDotBytes()
	if err != nil {
		return err
	}
	return reply(final)
}

func TestReplyCompositionAndValidation(t *testing.T) {
	var msg outboundMessage
	env := &imap.Envelope{Subject: "Hello\r\nBcc: hidden", From: []imap.Address{{Mailbox: "from", Host: "example.test"}}, ReplyTo: []imap.Address{{Mailbox: "replies", Host: "example.test"}}, MessageID: "thread@example.test"}
	if err := prepareReply(&msg, env); err != nil {
		t.Fatal(err)
	}
	wire, _, err := encodeMessage("sender@example.test", msg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"In-Reply-To: <thread@example.test>", "References: <thread@example.test>", "<replies@example.test>"} {
		if !bytes.Contains(wire, []byte(want)) {
			t.Errorf("missing %q", want)
		}
	}
	if bytes.Contains(wire, []byte("\r\nBcc:")) {
		t.Fatal("subject inserted a header")
	}
	for _, value := range []string{"to@example.test\r\nBcc:evil@example.test", "first@example.test, second@example.test", "<@example.test>"} {
		if _, err := parseRecipient(value); err == nil {
			t.Errorf("accepted invalid recipient %q", value)
		}
	}
	for _, file := range []outgoingAttachment{{Filename: "../path"}, {Filename: "a", ContentBase64: "invalid"}, {Filename: "a", ContentBase64: base64.StdEncoding.EncodeToString(make([]byte, maxAttachmentBytes+1))}} {
		if err := (composition{Attachments: []outgoingAttachment{file}}).parse(&outboundMessage{}); err == nil {
			t.Errorf("accepted invalid attachment %q", file.Filename)
		}
	}
}
