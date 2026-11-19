package main

import (
	"context"
	"errors"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type seenInput struct {
	messageInput
	Seen bool `json:"seen" jsonschema:"true marks read; false marks unread"`
}

type moveInput struct {
	messageInput
	Destination string `json:"destination" jsonschema:"existing destination folder in the same account"`
}

type moveResult struct {
	Destination string   `json:"destination"`
	UID         imap.UID `json:"uid,omitempty"`
	UIDValidity uint32   `json:"uid_validity,omitempty"`
}

func writeTool(name, description string, destructive, idempotent, external bool) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{
		DestructiveHint: &destructive, IdempotentHint: idempotent, OpenWorldHint: &external,
	}}
}

func selectForWrite(c *imapclient.Client, in messageInput) error {
	selected, err := c.Select(in.Folder, nil).Wait()
	if err != nil || selected.UIDValidity == 0 {
		return errors.New("could not open source folder for writing")
	}
	if selected.UIDValidity != in.UIDValidity {
		return errors.New("folder UID validity changed; search again before modifying")
	}
	return nil
}

func setSeen(c *imapclient.Client, in seenInput) error {
	if err := selectForWrite(c, in.messageInput); err != nil {
		return err
	}
	op := imap.StoreFlagsDel
	if in.Seen {
		op = imap.StoreFlagsAdd
	}
	results, err := c.Store(imap.UIDSetNum(in.UID), &imap.StoreFlags{Op: op, Flags: []imap.Flag{imap.FlagSeen}}, nil).Collect()
	if err != nil {
		return errors.New("could not set read status; check the folder before retrying")
	}
	for _, item := range results {
		if item.UID == in.UID {
			return nil
		}
	}
	return errors.New("server did not confirm the read status; search the folder again")
}

func selectableFolder(item *imap.ListData) bool {
	for _, attr := range item.Attrs {
		if strings.EqualFold(string(attr), `\Noselect`) || strings.EqualFold(string(attr), `\NonExistent`) {
			return false
		}
	}
	return true
}

func specialFolder(c *imapclient.Client, attr imap.MailboxAttr) (string, error) {
	var options *imap.ListOptions
	if c.Caps().Has(imap.CapSpecialUse) {
		options = &imap.ListOptions{ReturnSpecialUse: true}
	}
	cmd := c.List("", "*", options)
	var found string
	ambiguous := false
	for item := cmd.Next(); item != nil; item = cmd.Next() {
		if !selectableFolder(item) {
			continue
		}
		for _, a := range item.Attrs {
			if strings.EqualFold(string(a), string(attr)) {
				ambiguous = ambiguous || (found != "" && found != item.Mailbox)
				found = item.Mailbox
			}
		}
	}
	if err := cmd.Close(); err != nil {
		return "", errors.New("could not look up the destination folder")
	}
	if ambiguous || found == "" {
		return "", errors.New("no unique special-use folder found; configure the destination for this account")
	}
	return found, nil
}

func moveMessage(c *imapclient.Client, in messageInput, dest string) (moveResult, error) {
	if in.Folder == dest || (strings.EqualFold(in.Folder, "INBOX") && strings.EqualFold(dest, "INBOX")) {
		return moveResult{}, errors.New("message is already in the destination folder")
	}
	if err := selectForWrite(c, in); err != nil {
		return moveResult{}, err
	}
	uid := imap.UIDSetNum(in.UID)
	found, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{uid}}, nil).Wait()
	if err != nil {
		return moveResult{}, errors.New("could not verify the source message")
	}
	if uids, ok := found.All.(imap.UIDSet); !ok || !uids.Contains(in.UID) {
		return moveResult{}, errors.New("message not found in the source folder")
	}
	// The library otherwise falls back to COPY, STORE and potentially a folder-wide EXPUNGE.
	if !c.Caps().Has(imap.CapMove) {
		return moveResult{}, errors.New("mail server does not support safe IMAP MOVE")
	}
	data, err := c.Move(uid, dest).Wait()
	if err != nil {
		return moveResult{}, errors.New("move was not confirmed; check both folders before retrying")
	}
	out := moveResult{Destination: dest}
	src, srcOK := data.SourceUIDs.(imap.UIDSet)
	dst, dstOK := data.DestUIDs.(imap.UIDSet)
	if data.UIDValidity != 0 && srcOK && dstOK && src.Contains(in.UID) && len(dst) == 1 && dst[0].Start == dst[0].Stop {
		out.UID, out.UIDValidity = dst[0].Start, data.UIDValidity
	}
	return out, nil
}

func addMailboxWriteTools(s *mcp.Server, service *mailService) {
	mcp.AddTool(s, writeTool("set_message_read", "Set the read or unread status of one message.", false, true, false), func(ctx context.Context, _ *mcp.CallToolRequest, in seenInput) (*mcp.CallToolResult, struct {
		Seen bool `json:"seen"`
	}, error) {
		out := struct {
			Seen bool `json:"seen"`
		}{in.Seen}
		if err := in.validate(); err != nil {
			return nil, out, err
		}
		err := service.withClient(ctx, in.Account, func(c *imapclient.Client) error { return setSeen(c, in) })
		return nil, out, err
	})
	move := func(ctx context.Context, in messageInput, dest string, attr imap.MailboxAttr) (*mcp.CallToolResult, moveResult, error) {
		var out moveResult
		if err := in.validate(); err != nil {
			return nil, out, err
		}
		if attr == "" && dest == "" {
			return nil, out, errors.New("destination folder is required")
		}
		err := service.withClient(ctx, in.Account, func(c *imapclient.Client) error {
			var err error
			a := service.accounts[in.Account]
			switch attr {
			case imap.MailboxAttrArchive:
				dest = a.ArchiveFolder
			case imap.MailboxAttrTrash:
				dest = a.TrashFolder
			}
			if dest == "" {
				dest, err = specialFolder(c, attr)
				if err != nil {
					return err
				}
			}
			if dest, err = folderName(dest); err != nil {
				return err
			}
			out, err = moveMessage(c, in, dest)
			return err
		})
		return nil, out, err
	}
	mcp.AddTool(s, writeTool("move_message", "Move one message to an existing folder in the same account.", true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, in moveInput) (*mcp.CallToolResult, moveResult, error) {
		if _, err := folderName(in.Destination); err != nil {
			return nil, moveResult{}, err
		}
		return move(ctx, in.messageInput, in.Destination, "")
	})
	mcp.AddTool(s, writeTool("archive_message", "Move one message to this account's archive folder.", true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, in messageInput) (*mcp.CallToolResult, moveResult, error) {
		return move(ctx, in, "", imap.MailboxAttrArchive)
	})
	mcp.AddTool(s, writeTool("delete_message", "Move a message to Trash without permanently deleting it.", true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, in messageInput) (*mcp.CallToolResult, moveResult, error) {
		return move(ctx, in, "", imap.MailboxAttrTrash)
	})
}
