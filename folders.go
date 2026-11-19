package main

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func explicitFolder(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("an explicit folder name is required")
	}
	return folderName(name)
}

func managedFolder(name string) (string, error) {
	name, err := explicitFolder(name)
	if err != nil {
		return "", err
	}
	if sameFolder(name, "INBOX") {
		return "", errors.New("INBOX cannot be created, renamed or retired")
	}
	return name, nil
}

func hasSpecialUse(attrs []imap.MailboxAttr) bool {
	for _, a := range attrs {
		switch strings.ToLower(string(a)) {
		case `\all`, `\archive`, `\drafts`, `\flagged`, `\important`, `\junk`, `\sent`, `\trash`:
			return true
		}
	}
	return false
}

func validateFolderChange(c *imapclient.Client, src string, requireLeaf bool, protected []string) error {
	var opts *imap.ListOptions
	if c.Caps().Has(imap.CapSpecialUse) {
		opts = &imap.ListOptions{ReturnSpecialUse: true}
	}
	cmd := c.List("", "*", opts)
	var exists, hasDescendants, changesSpecial bool
	var delim rune
	for item := cmd.Next(); item != nil; item = cmd.Next() {
		if sameFolder(item.Mailbox, src) {
			exists = selectableFolder(item)
			delim = item.Delim
			changesSpecial = changesSpecial || hasSpecialUse(item.Attrs)
			for _, attr := range item.Attrs {
				hasDescendants = hasDescendants || strings.EqualFold(string(attr), `\HasChildren`)
			}
		} else if item.Delim != 0 && strings.HasPrefix(item.Mailbox, src+string(item.Delim)) {
			hasDescendants = true
			changesSpecial = changesSpecial || hasSpecialUse(item.Attrs)
		}
	}
	if err := cmd.Close(); err != nil || !exists {
		return errors.New("source folder could not be verified; refresh list_folders")
	}
	for _, name := range protected {
		changesSpecial = changesSpecial || strings.EqualFold(name, src) ||
			delim != 0 && strings.HasPrefix(strings.ToLower(name), strings.ToLower(src+string(delim)))
	}
	if changesSpecial {
		return errors.New("configured or special-use folders cannot be renamed or retired")
	}
	if requireLeaf && hasDescendants {
		return errors.New("only empty folders without subfolders can be retired")
	}
	return nil
}

func changeFolder(c *imapclient.Client, src, dest string, retire bool, protected []string) error {
	if err := validateFolderChange(c, src, retire, protected); err != nil {
		return err
	}
	if retire {
		data, err := c.Status(src, &imap.StatusOptions{NumMessages: true}).Wait()
		if err != nil || data.NumMessages == nil || *data.NumMessages != 0 {
			return errors.New("only a confirmed empty folder can be retired")
		}
	}
	if err := c.Rename(src, dest, nil).Wait(); err != nil {
		return errors.New("folder rename was not confirmed; refresh list_folders before retrying")
	}
	return nil
}

type folderEditInput struct {
	accountInput
	Folder string `json:"folder"`
}

type folderChangeResult struct {
	Folder string `json:"folder"`
}

func addFolderTools(s *mcp.Server, service *mailService) {
	mcp.AddTool(s, writeTool("create_folder", "Create a folder with an explicit name. Does not modify existing messages.", false, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, in folderEditInput) (*mcp.CallToolResult, folderChangeResult, error) {
		name, err := managedFolder(in.Folder)
		if err != nil {
			return nil, folderChangeResult{}, err
		}
		err = service.withClient(ctx, in.Account, func(c *imapclient.Client) error {
			if err := c.Create(name, nil).Wait(); err != nil {
				return errors.New("folder creation was not confirmed; refresh list_folders")
			}
			return nil
		})
		return nil, folderChangeResult{Folder: name}, err
	})
	change := func(ctx context.Context, in folderEditInput, dest string, retire bool) (*mcp.CallToolResult, folderChangeResult, error) {
		src, err := managedFolder(in.Folder)
		if err != nil {
			return nil, folderChangeResult{}, err
		}
		if retire {
			dest = "MCP-Retired-" + rand.Text()
		} else if dest, err = managedFolder(dest); err != nil {
			return nil, folderChangeResult{}, err
		}
		if sameFolder(src, dest) {
			return nil, folderChangeResult{}, errors.New("source and destination must be different folders")
		}
		err = service.withClient(ctx, in.Account, func(c *imapclient.Client) error {
			a := service.accounts[in.Account]
			return changeFolder(c, src, dest, retire, []string{a.ArchiveFolder, a.TrashFolder, a.DraftsFolder})
		})
		return nil, folderChangeResult{Folder: dest}, err
	}
	mcp.AddTool(s, writeTool("rename_folder", "Rename an ordinary folder. Descendants can move with it. INBOX and special-use folders are protected.", true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		folderEditInput
		Destination string `json:"destination"`
	}) (*mcp.CallToolResult, folderChangeResult, error) {
		return change(ctx, in.folderEditInput, in.Destination, false)
	})
	mcp.AddTool(s, writeTool("retire_empty_folder", "Remove an empty ordinary folder from its current name by renaming it to a recoverable MCP-Retired folder. Does not delete any folder or message.", true, false, false), func(ctx context.Context, _ *mcp.CallToolRequest, in folderEditInput) (*mcp.CallToolResult, folderChangeResult, error) {
		return change(ctx, in, "", true)
	})
}
