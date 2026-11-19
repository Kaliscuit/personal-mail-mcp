package main

import (
	"errors"
	"strconv"
	"strings"
)

const (
	maxFetchDepth = 64
	maxFetchLists = 4096
)

var errFetchLimit = errors.New("IMAP FETCH response exceeds complexity budget")

// The IMAP library recursively parses BODYSTRUCTURE. Bound FETCH nesting before
// it reaches that parser; quoted strings and size-delimited literals are opaque.
// This guard does not interpret IMAP values or replace protocol validation.
type fetchGuard struct {
	prefix           [48]byte
	prefixLen        int
	fetch, skipLine  bool
	quoted, escaped  bool
	depth, lists     int
	literalState     byte
	literalBytes     int
	literalHasDigits bool
}

func (g *fetchGuard) inspect(data []byte) error {
	for _, b := range data {
		if !g.fetch {
			if b == '\n' {
				g.prefixLen, g.skipLine = 0, false
			} else if !g.skipLine {
				switch {
				case b == '(':
					p := strings.Fields(string(g.prefix[:g.prefixLen]))
					if len(p) == 3 && p[0] == "*" && strings.EqualFold(p[2], "FETCH") {
						if n, err := strconv.ParseUint(p[1], 10, 32); err == nil && n != 0 {
							g.fetch, g.depth, g.lists = true, 1, 1
						}
					}
					g.skipLine = !g.fetch
				case g.prefixLen == len(g.prefix):
					g.skipLine = true
				default:
					g.prefix[g.prefixLen] = b
					g.prefixLen++
				}
			}
			continue
		}

		if g.literalState != 0 {
			if err := g.literal(b); err != nil {
				return err
			}
			continue
		}
		if g.quoted {
			switch {
			case g.escaped:
				g.escaped = false
			case b == '\\':
				g.escaped = true
			case b == '"':
				g.quoted = false
			}
			continue
		}
		switch b {
		case '"':
			g.quoted = true
		case '{':
			g.literalState, g.literalBytes, g.literalHasDigits = '{', 0, false
		case '(':
			g.depth++
			g.lists++
			if g.depth > maxFetchDepth || g.lists > maxFetchLists {
				return errFetchLimit
			}
		case ')':
			g.depth--
			if g.depth < 0 {
				return errFetchLimit
			}
		case '\n':
			*g = fetchGuard{}
		}
	}
	return nil
}

func (g *fetchGuard) literal(b byte) error {
	switch g.literalState {
	case '{':
		if b >= '0' && b <= '9' {
			g.literalHasDigits = true
			g.literalBytes = g.literalBytes*10 + int(b-'0')
			if g.literalBytes > maxIMAPBytes {
				return errFetchLimit
			}
			return nil
		}
		if g.literalHasDigits && (b == '+' || b == '}') {
			g.literalState = b
			return nil
		}
	case '+':
		if b == '}' {
			g.literalState = b
			return nil
		}
	case '}':
		if b == '\r' {
			g.literalState = b
			return nil
		}
	case '\r':
		if b == '\n' {
			g.literalState = 0
			if g.literalBytes > 0 {
				g.literalState = '\n'
			}
			return nil
		}
	case '\n':
		g.literalBytes--
		if g.literalBytes == 0 {
			g.literalState = 0
		}
		return nil
	}
	return errFetchLimit
}
