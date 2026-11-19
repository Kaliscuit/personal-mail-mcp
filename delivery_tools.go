package main

import (
	"bytes"
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type sendResult struct {
	MessageID string `json:"message_id"`
}

func prepareReply(msg *outboundMessage, env *imap.Envelope) error {
	if env == nil {
		return errors.New("original message has no reply information")
	}
	to := replyAddresses(env)
	for _, addr := range to {
		if !validEnvelopeAddress(addr) {
			return errors.New("original message has an unsupported reply address")
		}
	}
	if err := (recipients{To: to}).parse(msg); err != nil {
		return errors.New("original message does not have usable Reply-To or From addresses")
	}
	msg.subject = safeOriginalSubject("Re:", env.Subject)
	if validEnvelopeAddress(env.MessageID) {
		msg.replyID = env.MessageID
	}
	return nil
}

func prepareForward(c *imapclient.Client, msg *outboundMessage, original *imapclient.FetchMessageBuffer) error {
	if len(msg.attachments) >= maxAttachments {
		return errors.New("forwarding leaves room for at most 9 additional attachments")
	}
	remaining := maxAttachmentBytes
	for _, file := range msg.attachments {
		remaining -= len(file.data)
	}
	section := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Size: int64(remaining + 1)}}
	data, err := fetchMessageSection(c, original, section, remaining)
	if errors.Is(err, errSectionTooLarge) {
		return errors.New("original message and additional attachments exceed the 2 MiB forwarding limit")
	}
	if err != nil {
		return err
	}
	originalMessage, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return errors.New("could not verify original message headers for forwarding")
	}
	for name, values := range originalMessage.Header {
		name = strings.TrimRight(name, " \t")
		if !strings.EqualFold(name, "Bcc") && !strings.EqualFold(name, "Resent-Bcc") {
			continue
		}
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return errors.New("cannot forward a complete original that contains blind-recipient headers")
			}
		}
	}
	subject := ""
	if original.Envelope != nil {
		subject = original.Envelope.Subject
	}
	msg.subject = safeOriginalSubject("Fwd:", subject)
	// A base64-encoded message/rfc822 part would violate RFC 2046.
	msg.attachments = append(msg.attachments, binaryAttachment{"original.eml", "application/octet-stream", data})
	return nil
}

func addDeliveryTools(s *mcp.Server, service *mailService) {
	deliver := func(ctx context.Context, name string, msg outboundMessage, prepare func(context.Context, *outboundMessage) error) (*mcp.CallToolResult, sendResult, error) {
		a, ok := service.accounts[name]
		if !ok {
			return nil, sendResult{}, errors.New("unknown account; use list_accounts")
		}
		ctx, cancel := context.WithTimeout(ctx, 22*time.Second)
		defer cancel()
		if prepare != nil {
			if err := prepare(ctx, &msg); err != nil {
				return nil, sendResult{}, err
			}
		}
		out, err := a.submit(ctx, msg)
		return nil, out, err
	}
	mcp.AddTool(s, writeTool("send_message", "Send a new plain-text email from a configured account. Cc, Bcc and Base64 attachments are optional.", false, false, true), func(ctx context.Context, _ *mcp.CallToolRequest, in sendInput) (*mcp.CallToolResult, sendResult, error) {
		var msg outboundMessage
		if !cleanHeader(in.Subject, 512) {
			return nil, sendResult{}, errors.New("subject must be at most 512 bytes with no control characters")
		}
		msg.subject = in.Subject
		if err := in.recipients.parse(&msg); err != nil {
			return nil, sendResult{}, err
		}
		if err := in.composition.parse(&msg); err != nil {
			return nil, sendResult{}, err
		}
		return deliver(ctx, in.Account, msg, nil)
	})
	mcp.AddTool(s, writeTool("reply_message", "Reply to the original message's Reply-To (or From), not all recipients. Verify the original reply address before calling. The body is the supplied plain text.", false, false, true), func(ctx context.Context, _ *mcp.CallToolRequest, in replyInput) (*mcp.CallToolResult, sendResult, error) {
		var msg outboundMessage
		if err := in.validate(); err != nil {
			return nil, sendResult{}, err
		}
		if err := in.composition.parse(&msg); err != nil {
			return nil, sendResult{}, err
		}
		return deliver(ctx, in.Account, msg, func(ctx context.Context, msg *outboundMessage) error {
			return service.withClient(ctx, in.Account, func(c *imapclient.Client) error {
				original, err := messageMetadata(c, in.messageInput)
				if err != nil {
					return err
				}
				return prepareReply(msg, original.Envelope)
			})
		})
	})
	mcp.AddTool(s, writeTool("forward_message", "Forward a complete original message, including its attachments, as original.eml. To is required; Cc, Bcc and extra attachments are optional.", false, false, true), func(ctx context.Context, _ *mcp.CallToolRequest, in forwardInput) (*mcp.CallToolResult, sendResult, error) {
		var msg outboundMessage
		if err := in.validate(); err != nil {
			return nil, sendResult{}, err
		}
		if err := in.recipients.parse(&msg); err != nil {
			return nil, sendResult{}, err
		}
		if err := in.composition.parse(&msg); err != nil {
			return nil, sendResult{}, err
		}
		return deliver(ctx, in.Account, msg, func(ctx context.Context, msg *outboundMessage) error {
			return service.withClient(ctx, in.Account, func(c *imapclient.Client) error {
				original, err := messageMetadata(c, in.messageInput)
				if err != nil {
					return err
				}
				return prepareForward(c, msg, original)
			})
		})
	})
}

func envelopeAddresses(addresses []imap.Address) []string {
	out := make([]string, 0, len(addresses))
	for _, addr := range addresses {
		if value := addr.Addr(); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func replyAddresses(env *imap.Envelope) []string {
	if env == nil {
		return nil
	}
	if len(env.ReplyTo) > 0 {
		return envelopeAddresses(env.ReplyTo)
	}
	return envelopeAddresses(env.From)
}
