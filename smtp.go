package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"slices"
	"strings"
	"time"
)

type smtpEndpoint struct {
	address  string
	startTLS bool
}

var providerSMTP = map[string]smtpEndpoint{
	"gmail": {"smtp.gmail.com:465", false}, "workspace": {"smtp.gmail.com:465", false},
	"outlook": {"smtp.office365.com:587", true}, "yahoo": {"smtp.mail.yahoo.com:465", false},
	"icloud": {"smtp.mail.me.com:587", true}, "qq": {"smtp.qq.com:465", false},
}

type smtpOAuth xoauth2

func (x smtpOAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("XOAUTH2 requires TLS")
	}
	return xoauth2(x).Start()
}

func (x smtpOAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	return xoauth2(x).Next(challenge)
}

type smtpLogin struct {
	username, secret string
	step             int
}

func (a *smtpLogin) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("LOGIN requires TLS")
	}
	return "LOGIN", nil, nil
}

func (a *smtpLogin) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	switch a.step {
	case 1:
		return []byte(a.username), nil
	case 2:
		return []byte(a.secret), nil
	default:
		return nil, errors.New("unexpected SMTP authentication challenge")
	}
}

func authenticateSMTP(c *smtp.Client, a account, host, secret string) error {
	_, auth := c.Extension("AUTH")
	methods := strings.Fields(strings.ToUpper(auth))
	user := a.Username
	if a.Provider == "icloud" && !strings.Contains(user, "@") {
		user += "@icloud.com"
	}
	if a.Provider == "outlook" {
		if slices.Contains(methods, "XOAUTH2") && c.Auth(smtpOAuth{user, secret}) == nil {
			return nil
		}
		return errors.New("Outlook SMTP authentication failed; check SMTP AUTH and reauthorize with SMTP.Send permission")
	}
	var mechanism smtp.Auth
	switch {
	case slices.Contains(methods, "PLAIN"):
		mechanism = smtp.PlainAuth("", user, secret, host)
	case slices.Contains(methods, "LOGIN"):
		mechanism = &smtpLogin{username: user, secret: secret}
	default:
		return errors.New("mail server does not offer supported SMTP authentication over TLS")
	}
	if err := c.Auth(mechanism); err != nil {
		return errors.New("SMTP authentication failed; check the application credential and submission settings")
	}
	return nil
}

func (a *mailboxAccount) submit(ctx context.Context, msg outboundMessage) (sendResult, error) {
	wire, id, err := encodeMessage(a.FromAddress, msg)
	if err != nil {
		return sendResult{}, errors.New("could not encode the outgoing message")
	}
	secret, err := a.credential(ctx)
	if err != nil {
		return sendResult{}, err
	}
	host, _, _ := net.SplitHostPort(a.SMTPAddress)
	tlsConfig := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	c, err := openSMTP(ctx, a.SMTPAddress, a.SMTPTLS == "starttls", tlsConfig)
	if err != nil {
		return sendResult{}, err
	}
	defer c.Close()
	if err := authenticateSMTP(c.Client, a.account, host, secret); err != nil {
		return sendResult{}, err
	}
	if err := submitSMTP(c.Client, a.FromAddress, msg, wire); err != nil {
		return sendResult{}, err
	}
	return sendResult{MessageID: "<" + id + ">"}, nil
}

type smtpConn struct {
	*smtp.Client
	stop func() bool
}

func (c smtpConn) Close() error {
	defer c.stop()
	return c.Client.Close()
}

func openSMTP(ctx context.Context, endpoint string, startTLS bool, config *tls.Config) (smtpConn, error) {
	dialer := net.Dialer{Timeout: 8 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return smtpConn{}, errors.New("could not connect to the SMTP submission server")
	}
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	ok := false
	defer func() {
		if !ok {
			stop()
			_ = raw.Close()
		}
	}()
	if deadline, set := ctx.Deadline(); set {
		_ = raw.SetDeadline(deadline)
	}
	var conn net.Conn = &smtpResponseLimit{Conn: raw, remaining: 128 << 10}
	if !startTLS {
		tlsConn := tls.Client(conn, config)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return smtpConn{}, errors.New("could not establish verified SMTP TLS")
		}
		conn = tlsConn
	}
	c, err := smtp.NewClient(conn, config.ServerName)
	if err != nil {
		return smtpConn{}, errors.New("SMTP server did not send a valid greeting")
	}
	if startTLS {
		if supported, _ := c.Extension("STARTTLS"); !supported || c.StartTLS(config) != nil {
			return smtpConn{}, errors.New("SMTP server does not provide verified STARTTLS")
		}
	}
	ok = true
	return smtpConn{c, stop}, nil
}

type smtpResponseLimit struct {
	net.Conn
	remaining int
}

func (c *smtpResponseLimit) Read(p []byte) (int, error) {
	if c.remaining == 0 {
		return 0, errors.New("SMTP response limit reached")
	}
	n, err := c.Conn.Read(p[:min(len(p), c.remaining)])
	c.remaining -= n
	return n, err
}

func submitSMTP(c *smtp.Client, from string, msg outboundMessage, wire []byte) error {
	if err := c.Mail(from); err != nil {
		return errors.New("SMTP sender could not be accepted; nothing was submitted")
	}
	for _, list := range [][]*mail.Address{msg.to, msg.cc, msg.bcc} {
		for _, addr := range list {
			if err := c.Rcpt(addr.Address); err != nil {
				return errors.New("at least one SMTP recipient could not be accepted; nothing was submitted")
			}
		}
	}
	w, err := c.Data()
	if err != nil {
		return errors.New("SMTP did not accept message data; nothing was submitted")
	}
	if n, err := w.Write(wire); err != nil || n != len(wire) {
		// Do not close the DATA writer after a failed write: closing sends the terminator.
		return errors.New("SMTP submission was interrupted while writing; check before retrying")
	}
	if err := w.Close(); err != nil {
		var reply *textproto.Error
		if errors.As(err, &reply) && reply.Code >= 400 && reply.Code <= 599 {
			return errors.New("SMTP server rejected the message after DATA")
		}
		return errors.New("SMTP submission outcome is unknown; verify delivery before retrying")
	}
	return nil
}
