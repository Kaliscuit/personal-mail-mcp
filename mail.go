package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"mime"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
)

const (
	maxIMAPBytes = 8 << 20
	maxBodyBytes = 256 << 10
	maxBodyText  = 64 << 10
	maxFolders   = 500
)

type mailboxAccount struct {
	account
	refreshMu sync.Mutex
}

type mailService struct {
	accounts map[string]*mailboxAccount
}

func newMailService(c config) *mailService {
	s := &mailService{accounts: make(map[string]*mailboxAccount, len(c.Accounts))}
	for name, a := range c.Accounts {
		s.accounts[name] = &mailboxAccount{account: a}
	}
	return s
}

// The IMAP client sets per-command deadlines; never let them extend the request budget.
type limitedConn struct {
	net.Conn
	deadline  time.Time
	remaining int
	fetch     fetchGuard
}

func (c *limitedConn) Read(p []byte) (int, error) {
	if c.remaining == 0 {
		return 0, errors.New("IMAP response limit reached")
	}
	n, err := c.Conn.Read(p[:min(len(p), c.remaining)])
	c.remaining -= n
	if guardErr := c.fetch.inspect(p[:n]); guardErr != nil {
		return 0, guardErr
	}
	return n, err
}

func (c *limitedConn) limit(t time.Time) time.Time {
	if t.IsZero() || t.After(c.deadline) {
		return c.deadline
	}
	return t
}

func (c *limitedConn) SetDeadline(t time.Time) error      { return c.Conn.SetDeadline(c.limit(t)) }
func (c *limitedConn) SetReadDeadline(t time.Time) error  { return c.Conn.SetReadDeadline(c.limit(t)) }
func (c *limitedConn) SetWriteDeadline(t time.Time) error { return c.Conn.SetWriteDeadline(c.limit(t)) }

func (s *mailService) withClient(ctx context.Context, name string, run func(*imapclient.Client) error) error {
	a, ok := s.accounts[name]
	if !ok {
		return errors.New("unknown account; use list_accounts")
	}
	ctx, cancel := context.WithTimeout(ctx, 22*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	secret, err := a.credential(ctx)
	if err != nil {
		return err
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 8 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS12}}
	conn, err := dialer.DialContext(ctx, "tcp", a.Address)
	if err != nil {
		return errors.New("could not establish TLS connection to mailbox")
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	limited := &limitedConn{Conn: conn, deadline: deadline, remaining: maxIMAPBytes}
	_ = limited.SetDeadline(deadline)
	client := imapclient.New(limited, &imapclient.Options{WordDecoder: &mime.WordDecoder{CharsetReader: message.CharsetReader}})
	defer client.Close()
	if err := client.WaitGreeting(); err != nil {
		return errors.New("mail server did not send a valid greeting")
	}
	if a.Provider == "outlook" {
		err = client.Authenticate(xoauth2{a.Username, secret})
	} else {
		err = client.Login(a.Username, secret).Wait()
	}
	if err != nil {
		return errors.New("IMAP login failed; check account authorization and whether IMAP is enabled")
	}
	return run(client)
}

func (a *mailboxAccount) credential(ctx context.Context) (string, error) {
	if a.Provider == "outlook" {
		a.refreshMu.Lock()
		defer a.refreshMu.Unlock()
		if ctx.Err() != nil {
			return "", errors.New("mail operation timed out")
		}
		return outlookToken(ctx, a.account, microsoftConfig(a.account))
	}
	secret, err := readSecret(a.PasswordFile)
	if err != nil {
		return "", errors.New("mail account credential is unavailable")
	}
	return secret, nil
}

type folderList struct {
	Folders   []string `json:"folders"`
	Truncated bool     `json:"truncated"`
}

func listFolders(c *imapclient.Client) (folderList, error) {
	out := folderList{Folders: []string{}}
	cmd := c.List("", "*", nil)
	for item := cmd.Next(); item != nil; item = cmd.Next() {
		if !selectableFolder(item) {
			continue
		}
		if len(out.Folders) == maxFolders {
			out.Truncated = true
		} else {
			out.Folders = append(out.Folders, item.Mailbox)
		}
	}
	if err := cmd.Close(); err != nil {
		return out, errors.New("could not list mail folders")
	}
	sort.Strings(out.Folders)
	return out, nil
}

func examine(c *imapclient.Client, folder string, validity uint32) (*imap.SelectData, error) {
	selected, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil || selected.UIDValidity == 0 {
		return nil, errors.New("could not open folder read-only")
	}
	if validity != 0 && validity != selected.UIDValidity {
		return nil, errors.New("folder UID validity changed; search again before reading")
	}
	return selected, nil
}

type messageSummary struct {
	UID     imap.UID `json:"uid"`
	Subject string   `json:"subject"`
	From    []string `json:"from"`
	Date    string   `json:"date,omitempty"`
	Seen    bool     `json:"seen"`
}

func summarize(uid imap.UID, env *imap.Envelope) messageSummary {
	out := messageSummary{UID: uid, From: []string{}}
	if env != nil {
		out.Subject = env.Subject
		if !env.Date.IsZero() {
			out.Date = env.Date.Format(time.RFC3339)
		}
		out.From = envelopeAddresses(env.From)
	}
	return out
}

// Iterate the ends of UID ranges without expanding potentially large ranges.
func recentUIDs(set imap.UIDSet, before imap.UID, limit int) []imap.UID {
	var out []imap.UID
	for i := len(set) - 1; i >= 0 && len(out) < limit; i-- {
		r := set[i]
		if r.Start == 0 || r.Stop == 0 || (before != 0 && r.Start >= before) {
			continue
		}
		upper := r.Stop
		if before != 0 && upper >= before {
			upper = before - 1
		}
		for uid := upper; uid >= r.Start && len(out) < limit; uid-- {
			out = append(out, uid)
		}
	}
	return out
}

type searchResult struct {
	UIDValidity   uint32           `json:"uid_validity"`
	Messages      []messageSummary `json:"messages"`
	NextBeforeUID imap.UID         `json:"next_before_uid,omitempty"`
}

func findMessages(c *imapclient.Client, in searchInput, criteria imap.SearchCriteria) (searchResult, error) {
	selected, err := examine(c, in.Folder, in.UIDValidity)
	if err != nil {
		return searchResult{}, err
	}
	out := searchResult{UIDValidity: selected.UIDValidity, Messages: []messageSummary{}}
	if selected.NumMessages == 0 || in.BeforeUID == 1 {
		return out, nil
	}
	if in.BeforeUID > 1 {
		criteria.UID = []imap.UIDSet{{{Start: 1, Stop: in.BeforeUID - 1}}}
	}
	found, err := c.UIDSearch(&criteria, nil).Wait()
	if err != nil {
		return out, errors.New("mail search failed or exceeded the response budget; narrow the search")
	}
	set, ok := found.All.(imap.UIDSet)
	if !ok && found.All != nil {
		return out, errors.New("mail server returned an invalid UID search result")
	}
	uids := recentUIDs(set, in.BeforeUID, in.Limit+1)
	if len(uids) > in.Limit {
		uids = uids[:in.Limit]
		out.NextBeforeUID = uids[len(uids)-1]
	}
	if len(uids) == 0 {
		return out, nil
	}
	messages, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, Envelope: true, Flags: true}).Collect()
	if err != nil {
		return out, errors.New("could not fetch message summaries")
	}
	for _, item := range messages {
		summary := summarize(item.UID, item.Envelope)
		summary.Seen = hasSeen(item.Flags)
		out.Messages = append(out.Messages, summary)
	}
	sort.Slice(out.Messages, func(i, j int) bool { return out.Messages[i].UID > out.Messages[j].UID })
	return out, nil
}

func messageMetadata(c *imapclient.Client, in messageInput) (*imapclient.FetchMessageBuffer, error) {
	if _, err := examine(c, in.Folder, in.UIDValidity); err != nil {
		return nil, err
	}
	messages, err := c.Fetch(imap.UIDSetNum(in.UID), &imap.FetchOptions{
		UID: true, Envelope: true, Flags: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		return nil, errors.New("could not fetch message metadata")
	}
	for _, msg := range messages {
		if msg.UID == in.UID && msg.BodyStructure != nil {
			return msg, nil
		}
	}
	return nil, errors.New("message not found or has no body structure")
}

type attachment struct {
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	Size      uint32 `json:"encoded_size_bytes"`
	Part      []int  `json:"part"`
}

func isAttachment(p imap.BodyStructure) bool {
	if d := p.Disposition(); d != nil && (strings.EqualFold(d.Value, "attachment") || d.Params["filename"] != "") {
		return true
	}
	if part, ok := p.(*imap.BodyStructureSinglePart); ok {
		return part.Filename() != "" || part.MediaType() == "message/rfc822"
	}
	return false
}

func attachments(structure imap.BodyStructure) []attachment {
	items := []attachment{}
	walkAttachments(structure, func(path []int, part *imap.BodyStructureSinglePart) {
		items = append(items, attachmentInfo(path, part))
	})
	return items
}

type messageBody struct {
	messageSummary
	ReplyTo   []string `json:"reply_to,omitempty"`
	MediaType string   `json:"media_type,omitempty"`
	Body      string   `json:"body"`
	Truncated bool     `json:"truncated"`
	TooLarge  bool     `json:"body_too_large,omitempty"`
}

func selectBody(part imap.BodyStructure, path []int) (*imap.BodyStructureSinglePart, []int) {
	if isAttachment(part) {
		return nil, nil
	}
	switch p := part.(type) {
	case *imap.BodyStructureSinglePart:
		if p.MediaType() == "text/plain" || p.MediaType() == "text/html" {
			return p, append([]int(nil), path...)
		}
	case *imap.BodyStructureMultiPart:
		if len(p.Children) == 0 {
			return nil, nil
		}
		if strings.EqualFold(p.Subtype, "related") {
			root := 0
			if p.Extended != nil && p.Extended.Params["start"] != "" {
				start := strings.Trim(p.Extended.Params["start"], "<>")
				for i, child := range p.Children {
					if single, ok := child.(*imap.BodyStructureSinglePart); ok && strings.Trim(single.ID, "<>") == start {
						root = i
						break
					}
				}
			}
			return selectBody(p.Children[root], append(path, root+1))
		}
		var fallback *imap.BodyStructureSinglePart
		var fallbackPath []int
		for i, child := range p.Children {
			candidate, candidatePath := selectBody(child, append(path, i+1))
			if candidate != nil && candidate.MediaType() == "text/plain" {
				return candidate, candidatePath
			}
			if fallback == nil {
				fallback, fallbackPath = candidate, candidatePath
			}
		}
		return fallback, fallbackPath
	}
	return nil, nil
}

func hasSeen(flags []imap.Flag) bool {
	for _, flag := range flags {
		if strings.EqualFold(string(flag), string(imap.FlagSeen)) {
			return true
		}
	}
	return false
}

func readBody(c *imapclient.Client, msg *imapclient.FetchMessageBuffer) (messageBody, error) {
	out := messageBody{messageSummary: summarize(msg.UID, msg.Envelope)}
	out.Seen = hasSeen(msg.Flags)
	out.ReplyTo = replyAddresses(msg.Envelope)
	selected, path := selectBody(msg.BodyStructure, nil)
	if selected == nil {
		return out, nil
	}
	out.MediaType = selected.MediaType()
	if selected.Size > maxBodyBytes {
		out.TooLarge = true
		return out, nil
	}
	section := &imap.FetchItemBodySection{Part: path, Peek: true, Partial: &imap.SectionPartial{Size: maxBodyBytes + 1}}
	if _, single := msg.BodyStructure.(*imap.BodyStructureSinglePart); single {
		section.Part, section.Specifier = nil, imap.PartSpecifierText
	}
	body, err := fetchMessageSection(c, msg, section, maxBodyBytes)
	if errors.Is(err, errSectionTooLarge) {
		out.TooLarge = true
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var header message.Header
	header.Set("Content-Type", mime.FormatMediaType(out.MediaType, selected.Params))
	header.Set("Content-Transfer-Encoding", selected.Encoding)
	entity, err := message.New(header, bytes.NewReader(body))
	if err != nil {
		return out, errors.New("unsupported or invalid message encoding")
	}
	data, err := io.ReadAll(io.LimitReader(entity.Body, maxBodyText+utf8.UTFMax))
	if err != nil {
		return out, errors.New("could not decode message body")
	}
	text := strings.ToValidUTF8(string(data), "\uFFFD")
	if len(text) > maxBodyText {
		end := maxBodyText
		for !utf8.RuneStart(text[end]) {
			end--
		}
		text = text[:end]
		out.Truncated = true
	}
	out.Body = text
	return out, nil
}
