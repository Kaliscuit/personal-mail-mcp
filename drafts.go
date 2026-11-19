package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type saveDraftInput struct {
	accountInput
	composition
	To      []string `json:"to,omitempty"`
	Cc      []string `json:"cc,omitempty"`
	Bcc     []string `json:"bcc,omitempty"`
	Subject string   `json:"subject,omitempty"`
}

type draftResult struct {
	Folder      string   `json:"folder"`
	MessageID   string   `json:"message_id"`
	UID         imap.UID `json:"uid,omitempty"`
	UIDValidity uint32   `json:"uid_validity,omitempty"`
}

type listDraftsInput struct {
	accountInput
	searchFilters
	Limit       int      `json:"limit,omitempty"`
	BeforeUID   imap.UID `json:"before_uid,omitempty"`
	UIDValidity uint32   `json:"uid_validity,omitempty"`
}

// A UID is only meaningful in the folder that returned it; do not infer the
// source from the current configuration if a caller omits it.
type discardDraftInput struct {
	Account     string   `json:"account"`
	Folder      string   `json:"folder" jsonschema:"drafts folder returned by list_drafts or save_draft"`
	UID         imap.UID `json:"uid"`
	UIDValidity uint32   `json:"uid_validity"`
}

type draftPage struct {
	Folder string `json:"folder"`
	searchResult
}

func draftRoleFolder(c *imapclient.Client, configured string, role imap.MailboxAttr) (string, error) {
	if configured == "" {
		var err error
		configured, err = specialFolder(c, role)
		if err != nil {
			return "", err
		}
	}
	if configured == "" || strings.EqualFold(configured, "INBOX") {
		return "", errors.New("invalid special-use folder")
	}
	return folderName(configured)
}

func (in saveDraftInput) message() (outboundMessage, error) {
	var msg outboundMessage
	if !cleanHeader(in.Subject, 512) {
		return msg, errors.New("subject must be at most 512 bytes with no control characters")
	}
	msg.subject = in.Subject
	if err := (recipients{To: in.To, Cc: in.Cc, Bcc: in.Bcc}).parseLists(&msg); err != nil {
		return msg, err
	}
	err := in.composition.parse(&msg)
	return msg, err
}

func saveDraft(c *imapclient.Client, a account, msg outboundMessage) (draftResult, error) {
	folder, err := draftRoleFolder(c, a.DraftsFolder, imap.MailboxAttrDrafts)
	if err != nil {
		return draftResult{}, err
	}
	wire, id, err := encodeMIME(a.FromAddress, msg, true)
	if err != nil {
		return draftResult{}, errors.New("could not encode the draft")
	}
	out := draftResult{Folder: folder, MessageID: "<" + id + ">"}
	cmd := c.Append(folder, int64(len(wire)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagDraft}})
	n, writeErr := cmd.Write(wire)
	closeErr := cmd.Close()
	data, err := cmd.Wait()
	if writeErr != nil || n != len(wire) || closeErr != nil || err != nil {
		return out, fmt.Errorf("draft save not confirmed; check folder %q for Message-ID %s before retrying", folder, out.MessageID)
	}
	if data.UID != 0 && data.UIDValidity != 0 {
		out.UID, out.UIDValidity = data.UID, data.UIDValidity
	}
	return out, nil
}

func listDrafts(c *imapclient.Client, a account, in listDraftsInput) (draftPage, error) {
	folder, err := draftRoleFolder(c, a.DraftsFolder, imap.MailboxAttrDrafts)
	if err != nil {
		return draftPage{}, err
	}
	q := searchInput{mailboxScope: mailboxScope{Account: in.Account, Folder: folder}, searchFilters: in.searchFilters, Limit: in.Limit, BeforeUID: in.BeforeUID, UIDValidity: in.UIDValidity}
	criteria, err := q.validate()
	if err != nil {
		return draftPage{}, err
	}
	criteria.Flag = append(criteria.Flag, imap.FlagDraft)
	result, err := findMessages(c, q, criteria)
	return draftPage{folder, result}, err
}

func discardDraft(c *mailboxSession, a account, in messageInput) (moveResult, error) {
	if err := in.validate(); err != nil {
		return moveResult{}, err
	}
	folder, err := draftRoleFolder(c.Client, a.DraftsFolder, imap.MailboxAttrDrafts)
	if err != nil {
		return moveResult{}, err
	}
	if !sameFolder(in.Folder, folder) {
		return moveResult{}, errors.New("source folder must match this account's drafts folder")
	}
	dest, err := draftRoleFolder(c.Client, a.TrashFolder, imap.MailboxAttrTrash)
	if err != nil {
		return moveResult{}, err
	}
	return moveMessage(c, in, dest, imap.FlagDraft)
}

func addDraftTools(s *mcp.Server, service *mailService) {
	mcp.AddTool(s, writeTool("save_draft", "Save a new unsent draft in the account's drafts folder. Does not send or replace a message.", false, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, in saveDraftInput) (*mcp.CallToolResult, draftResult, error) {
		var out draftResult
		msg, err := in.message()
		if err != nil {
			return nil, out, err
		}
		err = service.withClient(ctx, in.Account, func(c *imapclient.Client) error {
			var err error
			out, err = saveDraft(c, service.accounts[in.Account].account, msg)
			return err
		})
		return nil, out, err
	})
	mcp.AddTool(s, readTool("list_drafts", "List messages in this account's drafts folder, newest UIDs first, without marking them read."), func(ctx context.Context, _ *mcp.CallToolRequest, in listDraftsInput) (*mcp.CallToolResult, draftPage, error) {
		var out draftPage
		err := service.withClient(ctx, in.Account, func(c *imapclient.Client) error {
			var err error
			out, err = listDrafts(c, service.accounts[in.Account].account, in)
			return err
		})
		return nil, out, err
	})
	mcp.AddTool(s, writeTool("discard_draft", "Move a draft to Trash using its folder, UID and UID validity from list_drafts. Never permanently deletes.", true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, in discardDraftInput) (*mcp.CallToolResult, moveResult, error) {
		var out moveResult
		err := service.withMailbox(ctx, in.Account, func(c *mailboxSession) error {
			var err error
			out, err = discardDraft(c, service.accounts[in.Account].account, messageInput(in))
			return err
		})
		return nil, out, err
	})
}
