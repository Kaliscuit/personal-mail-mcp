package main

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxMailboxScopes = 8

var keywordPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func keywordFlag(value string) (imap.Flag, error) {
	if !keywordPattern.MatchString(value) {
		return "", errors.New("keyword must be 1–64 ASCII letters, digits, periods, hyphens or underscores and start with a letter or digit")
	}
	return imap.Flag(value), nil
}

func setSummaryFlags(summary *messageSummary, flags []imap.Flag) {
	summary.Seen = hasSeen(flags)
	for _, flag := range flags {
		if strings.EqualFold(string(flag), string(imap.FlagFlagged)) {
			summary.Starred = true
		}
		if keywordPattern.MatchString(string(flag)) {
			summary.Keywords = append(summary.Keywords, string(flag))
		}
	}
}

func (s *mailboxScope) validate() error {
	if s.Account == "" {
		return errors.New("each scope requires an explicit account")
	}
	var err error
	s.Folder, err = folderName(s.Folder)
	return err
}

func validateScopeList(n int) error {
	if n == 0 || n > maxMailboxScopes {
		return errors.New("provide 1 to 8 explicit mailbox scopes")
	}
	return nil
}

func (s *mailService) withScope(ctx context.Context, scope mailboxScope, run func(*imapclient.Client) error) error {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return errors.New("request time budget exhausted before this scope")
	}
	return s.withClient(ctx, scope.Account, run)
}

type mailboxStatus struct {
	Messages    uint32 `json:"messages"`
	Unseen      uint32 `json:"unseen"`
	UIDValidity uint32 `json:"uid_validity"`
}

func readMailboxStatus(c *imapclient.Client, folder string) (mailboxStatus, error) {
	data, err := c.Status(folder, &imap.StatusOptions{NumMessages: true, NumUnseen: true, UIDValidity: true}).Wait()
	if err != nil || data.NumMessages == nil || data.NumUnseen == nil || data.UIDValidity == 0 {
		return mailboxStatus{}, errors.New("mail server did not return complete folder counts")
	}
	return mailboxStatus{*data.NumMessages, *data.NumUnseen, data.UIDValidity}, nil
}

type scopedResult[T any] struct {
	mailboxScope
	Result *T     `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type scopedResults[T any] struct {
	Scopes []scopedResult[T] `json:"scopes"`
}

type searchScopesInput struct {
	searchFilters
	Scopes []struct {
		mailboxScope
		BeforeUID   imap.UID `json:"before_uid,omitempty"`
		UIDValidity uint32   `json:"uid_validity,omitempty"`
	} `json:"scopes" jsonschema:"explicit accounts and folders to search (1 to 8), with per-folder pagination"`
	Limit int `json:"limit,omitempty" jsonschema:"results per scope (1 to 20, default 10)"`
}

func addMailboxReadTools(s *mcp.Server, service *mailService) {
	mcp.AddTool(s, readTool("get_mailbox_status", "Get total and unread counts for one or more explicit mailbox scopes. Each result can succeed or fail independently."), func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		Scopes []mailboxScope `json:"scopes" jsonschema:"explicit accounts and folders; omitted folders default to INBOX (1 to 8)"`
	}) (*mcp.CallToolResult, scopedResults[mailboxStatus], error) {
		var out scopedResults[mailboxStatus]
		if err := validateScopeList(len(in.Scopes)); err != nil {
			return nil, out, err
		}
		for _, scope := range in.Scopes {
			if err := scope.validate(); err != nil {
				return nil, out, err
			}
			item := scopedResult[mailboxStatus]{mailboxScope: scope}
			err := service.withScope(ctx, scope, func(c *imapclient.Client) error {
				result, err := readMailboxStatus(c, scope.Folder)
				if err == nil {
					item.Result = &result
				}
				return err
			})
			if err != nil {
				item.Error = err.Error()
			}
			out.Scopes = append(out.Scopes, item)
		}
		return nil, out, nil
	})
	mcp.AddTool(s, readTool("search_mailboxes", "Search explicit accounts and folders with the same filters. Returns an independently paginated result or error for each folder."), func(ctx context.Context, _ *mcp.CallToolRequest, in searchScopesInput) (*mcp.CallToolResult, scopedResults[searchResult], error) {
		var out scopedResults[searchResult]
		if err := validateScopeList(len(in.Scopes)); err != nil {
			return nil, out, err
		}
		criteria, err := in.searchFilters.criteria()
		if err != nil {
			return nil, out, err
		}
		if in.Limit == 0 {
			in.Limit = 10
		}
		if in.Limit < 1 || in.Limit > 20 {
			return nil, out, errors.New("per-folder limit must be 1 to 20")
		}
		for i := range in.Scopes {
			scope := &in.Scopes[i]
			if err := scope.mailboxScope.validate(); err != nil {
				return nil, out, err
			}
			if scope.BeforeUID != 0 && scope.UIDValidity == 0 {
				return nil, out, errors.New("pagination requires uid_validity in each scope")
			}
		}
		for _, scope := range in.Scopes {
			item := scopedResult[searchResult]{mailboxScope: scope.mailboxScope}
			query := searchInput{mailboxScope: scope.mailboxScope, Limit: in.Limit, BeforeUID: scope.BeforeUID, UIDValidity: scope.UIDValidity}
			err := service.withScope(ctx, scope.mailboxScope, func(c *imapclient.Client) error {
				result, err := findMessages(c, query, criteria)
				if err == nil {
					item.Result = &result
				}
				return err
			})
			if err != nil {
				item.Error = err.Error()
			}
			out.Scopes = append(out.Scopes, item)
		}
		return nil, out, nil
	})
}
