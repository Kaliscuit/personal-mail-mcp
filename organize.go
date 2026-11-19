package main

import (
	"context"
	"errors"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxBatchUIDs = 50

func sameFolder(a, b string) bool {
	return a == b || strings.EqualFold(a, "INBOX") && strings.EqualFold(b, "INBOX")
}

type batchInput struct {
	mailboxScope
	UIDs        []imap.UID `json:"uids" jsonschema:"1 to 50 distinct message UIDs from this folder"`
	UIDValidity uint32     `json:"uid_validity"`
}

func (in *batchInput) validate() (messageInput, error) {
	if err := in.mailboxScope.validate(); err != nil {
		return messageInput{}, err
	}
	if in.UIDValidity == 0 || len(in.UIDs) == 0 || len(in.UIDs) > maxBatchUIDs {
		return messageInput{}, errors.New("uid_validity and 1 to 50 distinct UIDs from the same folder are required")
	}
	seen := make(map[imap.UID]bool, len(in.UIDs))
	for _, uid := range in.UIDs {
		if uid == 0 || seen[uid] {
			return messageInput{}, errors.New("each UID must be positive and distinct")
		}
		seen[uid] = true
	}
	return messageInput{Account: in.Account, Folder: in.Folder, UIDValidity: in.UIDValidity}, nil
}

func verifiedUIDs(c *imapclient.Client, uids []imap.UID) (imap.UIDSet, error) {
	set := imap.UIDSetNum(uids...)
	found, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{set}}, nil).Wait()
	if err != nil {
		return nil, errors.New("could not verify the source messages")
	}
	existing, _ := found.All.(imap.UIDSet)
	for _, uid := range uids {
		if !existing.Contains(uid) {
			return nil, errors.New("at least one source message is missing; search the folder again")
		}
	}
	return set, nil
}

func moveSet(c *imapclient.Client, source messageInput, uids []imap.UID, destination string) (*imapclient.MoveData, error) {
	if sameFolder(source.Folder, destination) {
		return nil, errors.New("source and destination must be different folders")
	}
	if err := selectForWrite(c, source); err != nil {
		return nil, err
	}
	if !c.Caps().Has(imap.CapMove) {
		return nil, errors.New("mail server does not support safe IMAP MOVE")
	}
	set, err := verifiedUIDs(c, uids)
	if err != nil {
		return nil, err
	}
	data, err := c.Move(set, destination).Wait()
	if err != nil {
		return nil, errors.New("move was not confirmed; check both folders before retrying")
	}
	return data, nil
}

func copyMessage(c *imapclient.Client, in messageInput, destination string) (moveResult, error) {
	if sameFolder(in.Folder, destination) {
		return moveResult{}, errors.New("source and destination must be different folders")
	}
	if _, err := examine(c, in.Folder, in.UIDValidity); err != nil {
		return moveResult{}, err
	}
	set, err := verifiedUIDs(c, []imap.UID{in.UID})
	if err != nil {
		return moveResult{}, err
	}
	data, err := c.Copy(set, destination).Wait()
	if err != nil {
		return moveResult{}, errors.New("copy was not confirmed; check the destination before retrying")
	}
	out := moveResult{Destination: destination}
	if data.UIDValidity != 0 && data.SourceUIDs.Contains(in.UID) && len(data.DestUIDs) == 1 && data.DestUIDs[0].Start == data.DestUIDs[0].Stop {
		out.UID, out.UIDValidity = data.DestUIDs[0].Start, data.UIDValidity
	}
	return out, nil
}

type flagBatchInput struct {
	batchInput
	Seen           *bool    `json:"seen,omitempty"`
	Starred        *bool    `json:"starred,omitempty"`
	AddKeywords    []string `json:"add_keywords,omitempty"`
	RemoveKeywords []string `json:"remove_keywords,omitempty"`
}

type flagUpdate struct {
	add, remove []imap.Flag
}

func (in flagBatchInput) flags() (flagUpdate, error) {
	var out flagUpdate
	if len(in.AddKeywords)+len(in.RemoveKeywords) > 16 {
		return out, errors.New("at most 16 keyword changes are allowed")
	}
	for _, flag := range []struct {
		value *bool
		name  imap.Flag
	}{{in.Seen, imap.FlagSeen}, {in.Starred, imap.FlagFlagged}} {
		if flag.value != nil {
			if *flag.value {
				out.add = append(out.add, flag.name)
			} else {
				out.remove = append(out.remove, flag.name)
			}
		}
	}
	seen := make(map[string]bool)
	for _, edit := range []struct {
		keywords []string
		flags    *[]imap.Flag
	}{{in.AddKeywords, &out.add}, {in.RemoveKeywords, &out.remove}} {
		for _, keyword := range edit.keywords {
			flag, err := keywordFlag(keyword)
			if err != nil {
				return out, err
			}
			key := strings.ToLower(keyword)
			if seen[key] {
				return out, errors.New("each keyword can occur only once in a flag update")
			}
			seen[key] = true
			*edit.flags = append(*edit.flags, flag)
		}
	}
	if len(out.add)+len(out.remove) == 0 {
		return out, errors.New("provide at least one flag change")
	}
	return out, nil
}

func (update flagUpdate) matches(flags []imap.Flag) bool {
	hasFlag := func(wanted imap.Flag) bool {
		for _, flag := range flags {
			if strings.EqualFold(string(wanted), string(flag)) {
				return true
			}
		}
		return false
	}
	for _, flag := range update.add {
		if !hasFlag(flag) {
			return false
		}
	}
	for _, flag := range update.remove {
		if hasFlag(flag) {
			return false
		}
	}
	return true
}

type uidConfirmation struct {
	Confirmed   []imap.UID `json:"confirmed"`
	Unconfirmed []imap.UID `json:"unconfirmed"`
}

func confirmUIDs(uids []imap.UID, confirmed func(imap.UID) bool) uidConfirmation {
	out := uidConfirmation{Confirmed: []imap.UID{}, Unconfirmed: []imap.UID{}}
	for _, uid := range uids {
		if confirmed(uid) {
			out.Confirmed = append(out.Confirmed, uid)
		} else {
			out.Unconfirmed = append(out.Unconfirmed, uid)
		}
	}
	return out
}

func updateFlags(c *imapclient.Client, source messageInput, uids []imap.UID, update flagUpdate) (uidConfirmation, error) {
	if err := selectForWrite(c, source); err != nil {
		return uidConfirmation{}, err
	}
	set, err := verifiedUIDs(c, uids)
	if err != nil {
		return uidConfirmation{}, err
	}
	for _, change := range []imap.StoreFlags{{Op: imap.StoreFlagsAdd, Silent: true, Flags: update.add}, {Op: imap.StoreFlagsDel, Silent: true, Flags: update.remove}} {
		if len(change.Flags) > 0 {
			if err := c.Store(set, &change, nil).Close(); err != nil {
				return uidConfirmation{}, errors.New("flag update was not confirmed and may be partial; search the folder to check")
			}
		}
	}
	data, err := c.Fetch(set, &imap.FetchOptions{UID: true, Flags: true}).Collect()
	if err != nil {
		return uidConfirmation{}, errors.New("could not confirm flag changes; search the folder to check")
	}
	matches := make(map[imap.UID]bool, len(uids))
	for _, msg := range data {
		matches[msg.UID] = update.matches(msg.Flags)
	}
	return confirmUIDs(uids, func(uid imap.UID) bool { return matches[uid] }), nil
}

type batchMoveResult struct {
	uidConfirmation
	Destination string `json:"destination"`
}

func addOrganizeTools(s *mcp.Server, service *mailService) {
	mcp.AddTool(s, writeTool("copy_message", "Copy one message to another existing folder in the same account, leaving the original untouched.", false, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, in moveInput) (*mcp.CallToolResult, moveResult, error) {
		var out moveResult
		if err := in.validate(); err != nil {
			return nil, out, err
		}
		dest, err := explicitFolder(in.Destination)
		if err != nil {
			return nil, out, err
		}
		err = service.withClient(ctx, in.Account, func(c *imapclient.Client) error {
			var err error
			out, err = copyMessage(c, in.messageInput, dest)
			return err
		})
		return nil, out, err
	})
	mcp.AddTool(s, writeTool("update_message_flags", "Set read/starred state and add or remove IMAP custom keywords on explicitly selected messages in one folder. Other flags are preserved.", false, true, false), func(ctx context.Context, _ *mcp.CallToolRequest, in flagBatchInput) (*mcp.CallToolResult, uidConfirmation, error) {
		var out uidConfirmation
		source, err := in.batchInput.validate()
		if err != nil {
			return nil, out, err
		}
		update, err := in.flags()
		if err != nil {
			return nil, out, err
		}
		err = service.withClient(ctx, in.Account, func(c *imapclient.Client) error {
			var err error
			out, err = updateFlags(c, source, in.UIDs, update)
			return err
		})
		return nil, out, err
	})
	batchMove := func(ctx context.Context, in batchInput, destination string) (*mcp.CallToolResult, batchMoveResult, error) {
		var out batchMoveResult
		source, err := in.validate()
		if err != nil {
			return nil, out, err
		}
		err = service.withClient(ctx, in.Account, func(c *imapclient.Client) error {
			if destination == "" {
				destination = service.accounts[in.Account].ArchiveFolder
				if destination == "" {
					var err error
					destination, err = specialFolder(c, imap.MailboxAttrArchive)
					if err != nil {
						return err
					}
				}
			}
			data, err := moveSet(c, source, in.UIDs, destination)
			if err != nil {
				return err
			}
			out.Destination = destination
			copied, _ := data.SourceUIDs.(imap.UIDSet)
			out.uidConfirmation = confirmUIDs(in.UIDs, copied.Contains)
			return nil
		})
		return nil, out, err
	}
	mcp.AddTool(s, writeTool("move_messages", "Move up to 50 explicit UIDs from one folder to an existing folder in the same account. Requires native IMAP MOVE.", true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		batchInput
		Destination string `json:"destination"`
	}) (*mcp.CallToolResult, batchMoveResult, error) {
		destination, err := explicitFolder(in.Destination)
		if err != nil {
			return nil, batchMoveResult{}, err
		}
		return batchMove(ctx, in.batchInput, destination)
	})
	mcp.AddTool(s, writeTool("archive_messages", "Move up to 50 explicit UIDs from one folder to the account's archive folder. Requires native IMAP MOVE.", true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, in batchInput) (*mcp.CallToolResult, batchMoveResult, error) {
		return batchMove(ctx, in, "")
	})
}
