package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	mimemail "github.com/emersion/go-message/mail"
)

const (
	maxRecipients  = 25
	maxAttachments = 10
	maxSendText    = 64 << 10
)

type outgoingAttachment struct {
	Filename      string `json:"filename"`
	MediaType     string `json:"media_type,omitempty" jsonschema:"media type without parameters; defaults to application/octet-stream"`
	ContentBase64 string `json:"content_base64" jsonschema:"standard padded Base64 of the attachment bytes"`
}

type composition struct {
	Body        string               `json:"body" jsonschema:"plain text, not HTML"`
	Attachments []outgoingAttachment `json:"attachments,omitempty"`
}

type recipients struct {
	To  []string `json:"to" jsonschema:"recipient addresses, optionally with display names"`
	Cc  []string `json:"cc,omitempty"`
	Bcc []string `json:"bcc,omitempty"`
}

type sendInput struct {
	accountInput
	recipients
	composition
	Subject string `json:"subject"`
}

type replyInput struct {
	messageInput
	composition
}

type forwardInput struct {
	messageInput
	recipients
	composition
}

type binaryAttachment struct {
	name, mediaType string
	data            []byte
}

type outboundMessage struct {
	to, cc, bcc            []*mail.Address
	subject, body, replyID string
	attachments            []binaryAttachment
}

// Restrict envelope addresses to ASCII dot-atoms and DNS domains. MIME display
// names and subjects can still be Unicode.
var localPartPattern = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+(?:\\.[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+)*$")
var mediaTypePattern = regexp.MustCompile(`^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$`)

func cleanHeader(s string, maxBytes int) bool {
	return len(s) <= maxBytes && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) == -1
}

func validEnvelopeAddress(addr string) bool {
	local, domain, ok := strings.Cut(addr, "@")
	return ok && len(addr) <= 254 && len(local) <= 64 && localPartPattern.MatchString(local) && validDNSHostname(domain)
}

func parseRecipient(s string) (*mail.Address, error) {
	if cleanHeader(s, 512) {
		if addr, err := mail.ParseAddress(s); err == nil && cleanHeader(addr.Name, 256) && validEnvelopeAddress(addr.Address) {
			return addr, nil
		}
	}
	return nil, errors.New("invalid recipient; each list entry must contain one supported email address")
}

func (r recipients) parse(msg *outboundMessage) error {
	if len(r.To)+len(r.Cc)+len(r.Bcc) > maxRecipients || len(r.To) == 0 {
		return errors.New("provide at least one To recipient and no more than 25 recipients in total")
	}
	for _, field := range []struct {
		values []string
		dst    *[]*mail.Address
	}{{r.To, &msg.to}, {r.Cc, &msg.cc}, {r.Bcc, &msg.bcc}} {
		for _, value := range field.values {
			addr, err := parseRecipient(value)
			if err != nil {
				return err
			}
			*field.dst = append(*field.dst, addr)
		}
	}
	return nil
}

func (in composition) parse(msg *outboundMessage) error {
	if len(in.Body) > maxSendText || !utf8.ValidString(in.Body) || strings.ContainsRune(in.Body, '\x00') {
		return errors.New("body must be UTF-8 plain text of at most 64 KiB without NUL")
	}
	if len(in.Attachments) > maxAttachments {
		return errors.New("too many attachments; at most 10 are allowed")
	}
	msg.body = strings.ReplaceAll(strings.ReplaceAll(in.Body, "\r\n", "\n"), "\r", "\n")
	total := 0
	for _, file := range in.Attachments {
		if file.Filename == "" || !cleanHeader(file.Filename, 200) || strings.ContainsAny(file.Filename, "/\\") || file.Filename == "." || file.Filename == ".." {
			return errors.New("attachment filename must be a safe non-empty basename")
		}
		mediaType := strings.ToLower(file.MediaType)
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		if len(mediaType) > 120 || !mediaTypePattern.MatchString(mediaType) || strings.HasPrefix(mediaType, "multipart/") || strings.HasPrefix(mediaType, "message/") {
			return errors.New("invalid or unsupported attachment media type")
		}
		if len(file.ContentBase64) > base64.StdEncoding.EncodedLen(maxAttachmentBytes-total) || strings.ContainsAny(file.ContentBase64, "\r\n") {
			return errors.New("attachments exceed the total size limit or contain invalid Base64")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(file.ContentBase64)
		if err != nil {
			return errors.New("invalid attachment Base64")
		}
		total += len(data)
		if total > maxAttachmentBytes {
			return errors.New("attachments exceed the total size limit")
		}
		msg.attachments = append(msg.attachments, binaryAttachment{file.Filename, mediaType, data})
	}
	return nil
}

func safeOriginalSubject(prefix, subject string) string {
	subject = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, subject)
	if len(subject) > 512 {
		end := 512
		for !utf8.RuneStart(subject[end]) {
			end--
		}
		subject = subject[:end]
	}
	if strings.HasPrefix(strings.ToLower(subject), strings.ToLower(prefix)) {
		return subject
	}
	return prefix + " " + subject
}

func encodeMessage(from string, msg outboundMessage) ([]byte, string, error) {
	var h mimemail.Header
	h.SetDate(time.Now())
	_, domain, _ := strings.Cut(from, "@")
	if err := h.GenerateMessageIDWithHostname(domain); err != nil {
		return nil, "", err
	}
	id, err := h.MessageID()
	if err != nil {
		return nil, "", err
	}
	h.SetAddressList("From", []*mail.Address{{Address: from}})
	h.SetAddressList("To", msg.to)
	if len(msg.cc) != 0 {
		h.SetAddressList("Cc", msg.cc)
	}
	h.SetSubject(msg.subject)
	if msg.replyID != "" {
		h.SetMsgIDList("In-Reply-To", []string{msg.replyID})
		h.SetMsgIDList("References", []string{msg.replyID})
	}
	var out bytes.Buffer
	const plain = "text/plain; charset=utf-8"
	if len(msg.attachments) == 0 {
		h.Set("Content-Type", plain)
		w, err := mimemail.CreateSingleInlineWriter(&out, h)
		if err != nil {
			return nil, "", err
		}
		if err := writePart(w, []byte(msg.body)); err != nil {
			return nil, "", err
		}
		return out.Bytes(), id, nil
	}
	w, err := mimemail.CreateWriter(&out, h)
	if err != nil {
		return nil, "", err
	}
	var textHeader mimemail.InlineHeader
	textHeader.Set("Content-Type", plain)
	text, err := w.CreateSingleInline(textHeader)
	if err != nil {
		return nil, "", err
	}
	if err := writePart(text, []byte(msg.body)); err != nil {
		return nil, "", err
	}
	for _, file := range msg.attachments {
		var ah mimemail.AttachmentHeader
		ah.Set("Content-Type", file.mediaType)
		ah.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.name}))
		part, err := w.CreateAttachment(ah)
		if err != nil {
			return nil, "", err
		}
		if err := writePart(part, file.data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return out.Bytes(), id, nil
}

func writePart(w io.WriteCloser, data []byte) error {
	_, err := w.Write(data)
	return errors.Join(err, w.Close())
}
