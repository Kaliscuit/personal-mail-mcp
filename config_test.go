package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadAdditionalAllowedHosts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hosts []string
		want  []string
		bad   bool
	}{
		{name: "omitted"},
		{name: "configured", hosts: []string{"Mail.Example.Test", "mcp-2.example.test"}, want: []string{"mail.example.test", "mcp-2.example.test"}},
		{name: "empty", hosts: []string{""}, bad: true},
		{name: "wildcard", hosts: []string{"*.example.test"}, bad: true},
		{name: "port", hosts: []string{"mail.example.test:443"}, bad: true},
		{name: "url", hosts: []string{"https://mail.example.test/mcp"}, bad: true},
		{name: "ip", hosts: []string{"127.0.0.1"}, bad: true},
		{name: "empty label", hosts: []string{"mail..example.test"}, bad: true},
		{name: "trailing dot", hosts: []string{"mail.example.test."}, bad: true},
		{name: "invalid label", hosts: []string{"-mail.example.test"}, bad: true},
		{name: "label too long", hosts: []string{strings.Repeat("a", 64) + ".example.test"}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data, err := json.Marshal(config{
				Interfaces: []string{"lo"}, Port: 8080, TokenFile: "token",
				AdditionalAllowedHosts: tc.hosts,
				Accounts: map[string]account{
					"mail": {Provider: "gmail", Username: "mail@example.test", PasswordFile: "password"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(path)
			if tc.bad {
				if err == nil || !strings.Contains(err.Error(), "additional_allowed_hosts[0]") {
					t.Fatalf("expected allowlist config error, got %v", err)
				}
				return
			}
			if err != nil || !slices.Equal(cfg.AdditionalAllowedHosts, tc.want) {
				t.Fatalf("allowlist = %v, err = %v; want %v", cfg.AdditionalAllowedHosts, err, tc.want)
			}
		})
	}
}
