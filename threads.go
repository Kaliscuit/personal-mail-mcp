package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/textproto"
	"slices"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	threadFolders     = 4
	threadPivots      = 8
	threadCandidates  = 24 // per folder and pivot
	threadResults     = 32
	threadHeaderBytes = 8 << 10
)

var threadFields = []string{"Message-Id", "In-Reply-To", "References"}

type threadInput struct {
	messageInput
	Folders []string `json:"folders,omitempty" jsonschema:"additional folders in the same account (up to three); defaults to the anchor folder only"`
}

type threadMember struct {
	messageInput
	MessageID string `json:"message_id,omitempty"`
	Subject   string `json:"subject,omitempty"`
}

type threadResult struct {
	Messages  []threadMember `json:"messages"`
	Truncated bool           `json:"truncated"`
	Warning   string         `json:"warning,omitempty"`
}

func (in *threadInput) scope() ([]string, error) {
	if err := in.messageInput.validate(); err != nil {
		return nil, err
	}
	if len(in.Folders) >= threadFolders {
		return nil, errors.New("at most three additional thread folders are allowed")
	}
	folders := []string{in.Folder}
	for _, name := range in.Folders {
		f, err := folderName(name)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(folders, func(s string) bool { return s == f || strings.EqualFold(f, "INBOX") && strings.EqualFold(s, "INBOX") }) {
			folders = append(folders, f)
		}
	}
	return folders, nil
}

type threadHeader struct {
	threadMember
	ids     []string
	partial bool
}

func parseThreadHeader(ref messageInput, data []byte) threadHeader {
	h := threadHeader{threadMember: threadMember{messageInput: ref}}
	if len(data) > threadHeaderBytes || data == nil {
		h.partial = true
		return h
	}
	fields, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(data))).ReadMIMEHeader()
	if err != nil {
		h.partial = true
		return h
	}
	header := mail.HeaderFromMap(fields)
	h.Subject, _ = header.Subject()
	for i, key := range threadFields {
		ids, err := header.MsgIDList(key)
		if err != nil || len(fields.Values(key)) > 1 {
			h.partial = true
		}
		if i == 0 && len(ids) > 1 {
			h.partial = true
			continue
		}
		for _, id := range ids {
			if len(id) > 512 {
				h.partial = true
				continue
			}
			id = "<" + id + ">"
			h.ids = append(h.ids, id)
			if i == 0 {
				h.MessageID = id
			}
		}
	}
	return h
}

// Resolve UIDs before fetching literals: a server may return BODY before UID.
// Verify each sequence number's UID again in case of an intervening expunge.
func threadHeaders(c *imapclient.Client, ref messageInput, uids []imap.UID) ([]threadHeader, error) {
	meta, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		return nil, errors.New("could not locate thread messages")
	}
	var seq imap.SeqSet
	expected := map[uint32]imap.UID{}
	for _, m := range meta {
		if slices.Contains(uids, m.UID) {
			seq.AddNum(m.SeqNum)
			expected[m.SeqNum] = m.UID
		}
	}
	if len(seq) == 0 {
		return nil, nil
	}
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Message-Id", "In-Reply-To", "References", "Subject"}, Peek: true, Partial: &imap.SectionPartial{Size: threadHeaderBytes + 1}}
	msgs, err := c.Fetch(seq, &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		return nil, errors.New("could not fetch thread headers")
	}
	var out []threadHeader
	for _, m := range msgs {
		if m.UID != 0 && expected[m.SeqNum] == m.UID {
			ref.UID = m.UID
			out = append(out, parseThreadHeader(ref, m.FindBodySection(section)))
		}
	}
	return out, nil
}

func threadSearch(c *imapclient.Client, id string) ([]imap.UID, error) {
	header := func(key string) imap.SearchCriteria {
		return imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: key, Value: id}}}
	}
	or := func(a, b imap.SearchCriteria) imap.SearchCriteria {
		return imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{a, b}}}
	}
	criteria := or(header(threadFields[0]), or(header(threadFields[1]), header(threadFields[2])))
	found, err := c.UIDSearch(&criteria, nil).Wait()
	if err != nil {
		return nil, errors.New("thread header search failed or exceeded the response budget")
	}
	set, ok := found.All.(imap.UIDSet)
	if !ok && found.All != nil {
		return nil, errors.New("mail server returned an invalid UID search result")
	}
	return recentUIDs(set, 0, threadCandidates+1), nil
}

func lookupThread(c *imapclient.Client, in threadInput, folders []string) (threadResult, error) {
	var out threadResult
	if _, err := examine(c, in.Folder, in.UIDValidity); err != nil {
		return out, err
	}
	anchor, err := threadHeaders(c, in.messageInput, []imap.UID{in.UID})
	if err != nil {
		return out, err
	}
	if len(anchor) != 1 {
		return out, errors.New("anchor message not found")
	}
	var pivots []string
	seen := map[messageInput]bool{}
	add := func(h threadHeader) {
		seen[h.messageInput] = true
		out.Messages = append(out.Messages, h.threadMember)
		for _, id := range h.ids {
			if !slices.Contains(pivots, id) {
				if len(pivots) == threadPivots {
					out.Truncated = true
					continue
				}
				pivots = append(pivots, id)
			}
		}
	}
	add(anchor[0])
	out.Truncated = out.Truncated || anchor[0].partial
	validity := map[string]uint32{in.Folder: in.UIDValidity}
	selected := in.Folder
	partial := func(err error) (threadResult, error) {
		out.Truncated, out.Warning = true, err.Error()
		return out, nil
	}
	for i := 0; i < len(pivots); i++ {
		for _, folder := range folders {
			if selected != folder {
				s, err := examine(c, folder, validity[folder])
				if err != nil {
					return partial(err)
				}
				validity[folder] = s.UIDValidity
				selected = folder
			}
			uids, err := threadSearch(c, pivots[i])
			if err != nil {
				return partial(err)
			}
			if len(uids) > threadCandidates {
				uids = uids[:threadCandidates]
				out.Truncated = true
			}
			ref := in.messageInput
			ref.Folder, ref.UIDValidity = folder, validity[folder]
			uids = slices.DeleteFunc(uids, func(uid imap.UID) bool { ref.UID = uid; return seen[ref] })
			if len(uids) == 0 {
				continue
			}
			headers, err := threadHeaders(c, ref, uids)
			if err != nil {
				return partial(err)
			}
			if len(headers) < len(uids) {
				out.Truncated = true
			}
			for _, h := range headers {
				out.Truncated = out.Truncated || h.partial
				// SEARCH HEADER is a case-insensitive substring match.
				if !slices.Contains(h.ids, pivots[i]) {
					continue
				}
				if len(out.Messages) == threadResults {
					out.Truncated = true
					return out, nil
				}
				add(h)
			}
		}
	}
	return out, nil
}

func addThreadTool(s *mcp.Server, service *mailService) {
	mcp.AddTool(s, readTool("get_thread", "Find messages linked to an anchor by exact Message-ID, In-Reply-To or References in the requested folders. Anchor first; truncated indicates an incomplete lookup."), func(ctx context.Context, _ *mcp.CallToolRequest, in threadInput) (*mcp.CallToolResult, threadResult, error) {
		var out threadResult
		folders, err := in.scope()
		if err != nil {
			return nil, out, err
		}
		err = service.withClient(ctx, in.Account, func(c *imapclient.Client) error {
			var err error
			out, err = lookupThread(c, in, folders)
			return err
		})
		return nil, out, err
	})
}
