package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type config struct {
	Interfaces             []string           `json:"interfaces"`
	Port                   int                `json:"port"`
	AdditionalAllowedHosts []string           `json:"additional_allowed_hosts,omitempty"`
	TokenFile              string             `json:"token_file"`
	Accounts               map[string]account `json:"accounts"`
}

type account struct {
	Provider      string `json:"provider"`
	Address       string `json:"address,omitempty"`
	Username      string `json:"username"`
	PasswordFile  string `json:"password_file,omitempty"`
	ClientID      string `json:"client_id,omitempty"`
	Tenant        string `json:"tenant,omitempty"`
	OAuthFile     string `json:"oauth_file,omitempty"`
	ArchiveFolder string `json:"archive_folder,omitempty"`
	TrashFolder   string `json:"trash_folder,omitempty"`
}

var providerAddress = map[string]string{
	"gmail": "imap.gmail.com:993", "workspace": "imap.gmail.com:993",
	"outlook": "outlook.office365.com:993", "yahoo": "imap.mail.yahoo.com:993",
	"icloud": "imap.mail.me.com:993", "qq": "imap.qq.com:993",
}

func decodeFile(path string, target any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("file could not be read or exceeds 1 MiB")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("unexpected data after JSON object")
	}
	return nil
}

func loadConfig(path string) (config, error) {
	var c config
	if err := decodeFile(path, &c); err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	if len(c.Interfaces) == 0 || c.Port < 1 || c.Port > 65535 || c.TokenFile == "" || len(c.Accounts) == 0 {
		return c, errors.New("interfaces, valid port, token_file and accounts are required")
	}
	for i, host := range c.AdditionalAllowedHosts {
		if !validDNSHostname(host) {
			return c, fmt.Errorf("additional_allowed_hosts[%d] must be a DNS hostname without a port", i)
		}
		c.AdditionalAllowedHosts[i] = strings.ToLower(host)
	}
	base := filepath.Dir(path)
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	c.TokenFile = resolve(c.TokenFile)
	oauthFiles := make(map[string]bool)
	for name, a := range c.Accounts {
		addr, ok := providerAddress[a.Provider]
		if strings.TrimSpace(name) == "" || strings.TrimSpace(a.Username) == "" || !ok {
			return c, fmt.Errorf("account %q requires a username and valid provider", name)
		}
		for _, folder := range []string{a.ArchiveFolder, a.TrashFolder} {
			if _, err := folderName(folder); err != nil || strings.EqualFold(folder, "INBOX") {
				return c, fmt.Errorf("account %q has an invalid archive or trash folder", name)
			}
		}
		if a.ArchiveFolder != "" && a.ArchiveFolder == a.TrashFolder {
			return c, fmt.Errorf("account %q must use distinct archive and trash folders", name)
		}
		if a.Address == "" {
			a.Address = addr
		}
		if a.Provider == "outlook" {
			if a.OAuthFile == "" || a.ClientID == "" || a.PasswordFile != "" || a.Address != addr {
				return c, fmt.Errorf("account %q requires oauth_file, client_id and the default Outlook IMAP host", name)
			}
			if a.Tenant == "" {
				a.Tenant = "consumers"
			}
			if !validTenant(a.Tenant) {
				return c, fmt.Errorf("account %q has an invalid tenant", name)
			}
			a.OAuthFile = resolve(a.OAuthFile)
			if oauthFiles[a.OAuthFile] || a.OAuthFile == c.TokenFile {
				return c, fmt.Errorf("account %q requires a unique oauth_file", name)
			}
			oauthFiles[a.OAuthFile] = true
		} else if a.PasswordFile == "" || a.OAuthFile != "" || a.ClientID != "" || a.Tenant != "" {
			return c, fmt.Errorf("account %q requires password_file and no OAuth settings", name)
		}
		host, p, err := net.SplitHostPort(a.Address)
		n, portErr := strconv.Atoi(p)
		if err != nil || host == "" || portErr != nil || n < 1 || n > 65535 {
			return c, fmt.Errorf("account %q has an invalid IMAP address", name)
		}
		a.PasswordFile = resolve(a.PasswordFile)
		c.Accounts[name] = a
	}
	for name, a := range c.Accounts {
		if a.PasswordFile != "" && (oauthFiles[a.PasswordFile] || a.PasswordFile == c.TokenFile) {
			return c, fmt.Errorf("account %q must not share password_file with token files", name)
		}
	}
	return c, nil
}

func validDNSHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func validTenant(s string) bool {
	if len(s) > 253 || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-", r) {
			return false
		}
	}
	return s != ""
}

func readSecret(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", errors.New("secret could not be read or exceeds 4096 bytes")
	}
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" || strings.ContainsAny(s, "\r\n\x00") {
		return "", errors.New("secret must contain one non-empty line")
	}
	return s, nil
}

func tokenHash(path string) ([32]byte, error) {
	token, err := readSecret(path)
	if err != nil {
		return [32]byte{}, err
	}
	if len(token) < 32 || strings.IndexFunc(token, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
		return [32]byte{}, errors.New("MCP token must contain at least 32 printable ASCII characters without spaces")
	}
	return sha256.Sum256([]byte(token)), nil
}

func listenAddresses(interfaces []string, port int) ([]string, error) {
	var result []string
	seen := make(map[string]bool)
	for _, name := range interfaces {
		iface, err := net.InterfaceByName(name)
		if err != nil || iface.Flags&net.FlagUp == 0 {
			return nil, fmt.Errorf("interface %q is missing or down", name)
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, fmt.Errorf("read addresses for interface %q: %w", name, err)
		}
		usable := false
		for _, addr := range addrs {
			prefix, err := netip.ParsePrefix(addr.String())
			if err != nil {
				continue
			}
			ip := prefix.Addr().Unmap()
			if !ip.IsGlobalUnicast() && !ip.IsLoopback() {
				continue
			}
			usable = true
			endpoint := net.JoinHostPort(ip.String(), strconv.Itoa(port))
			if !seen[endpoint] {
				result = append(result, endpoint)
				seen[endpoint] = true
			}
		}
		if !usable {
			return nil, fmt.Errorf("interface %q has no usable address", name)
		}
	}
	return result, nil
}
