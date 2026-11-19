package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime/quotedprintable"
	"slices"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	maxAttachmentBytes = 2 << 20
	maxAttachmentWire  = 6 << 20
)

type attachmentInput struct {
	messageInput
	Part []int `json:"part" jsonschema:"MIME part identifier from list_attachments"`
}

type attachmentDownload struct {
	attachment
	ContentBase64 string `json:"content_base64"`
}

func walkAttachments(structure imap.BodyStructure, visit func([]int, *imap.BodyStructureSinglePart)) {
	structure.Walk(func(path []int, p imap.BodyStructure) bool {
		if part, ok := p.(*imap.BodyStructureSinglePart); ok && isAttachment(part) {
			visit(path, part)
		}
		return true
	})
}

func attachmentInfo(path []int, p *imap.BodyStructureSinglePart) attachment {
	return attachment{Filename: p.Filename(), MediaType: p.MediaType(), Size: p.Size, Part: slices.Clone(path)}
}

func downloadAttachment(c *imapclient.Client, msg *imapclient.FetchMessageBuffer, path []int) (attachmentDownload, error) {
	var part *imap.BodyStructureSinglePart
	walkAttachments(msg.BodyStructure, func(candidate []int, p *imap.BodyStructureSinglePart) {
		if slices.Equal(path, candidate) {
			part = p
		}
	})
	if part == nil {
		return attachmentDownload{}, errors.New("attachment part not found; use list_attachments")
	}
	if part.Size > maxAttachmentWire {
		return attachmentDownload{}, errors.New("attachment exceeds the encoded download limit")
	}
	section := &imap.FetchItemBodySection{Part: path, Peek: true, Partial: &imap.SectionPartial{Size: maxAttachmentWire + 1}}
	if _, single := msg.BodyStructure.(*imap.BodyStructureSinglePart); single {
		section.Part, section.Specifier = nil, imap.PartSpecifierText
	}
	raw, err := fetchMessageSection(c, msg, section, maxAttachmentWire)
	if err != nil {
		return attachmentDownload{}, err
	}
	var decoded io.Reader = bytes.NewReader(raw)
	switch strings.ToLower(part.Encoding) {
	case "", "7bit", "8bit", "binary":
	case "base64":
		decoded = base64.NewDecoder(base64.StdEncoding, decoded)
	case "quoted-printable":
		decoded = quotedprintable.NewReader(decoded)
	default:
		return attachmentDownload{}, errors.New("unsupported attachment transfer encoding")
	}
	body, err := io.ReadAll(io.LimitReader(decoded, maxAttachmentBytes+1))
	if err != nil {
		return attachmentDownload{}, errors.New("could not decode attachment")
	}
	if len(body) > maxAttachmentBytes {
		return attachmentDownload{}, errors.New("attachment exceeds the decoded download limit")
	}
	return attachmentDownload{attachmentInfo(path, part), base64.StdEncoding.EncodeToString(body)}, nil
}

var errSectionTooLarge = errors.New("message data exceeds the download limit")

func fetchMessageSection(c *imapclient.Client, msg *imapclient.FetchMessageBuffer, section *imap.FetchItemBodySection, maxBytes int) ([]byte, error) {
	// Match by sequence number when the server sends the literal before the UID,
	// but verify the UID before using any returned bytes.
	fetched, err := c.Fetch(imap.SeqSetNum(msg.SeqNum), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		return nil, errors.New("could not fetch message data")
	}
	for _, m := range fetched {
		if m.UID != msg.UID {
			continue
		}
		if data := m.FindBodySection(section); data != nil {
			if len(data) > maxBytes {
				return nil, errSectionTooLarge
			}
			return data, nil
		}
	}
	return nil, errors.New("message data is no longer available")
}
