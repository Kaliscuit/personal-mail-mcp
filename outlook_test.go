package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestOutlookRefreshPersistsRotation(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.FormValue("grant_type") != "refresh_token" || r.FormValue("refresh_token") != "old-refresh" || r.FormValue("client_id") != "test-app" {
			http.Error(w, "invalid test request", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "rotated-refresh", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer s.Close()
	a := account{Username: "test@example.net", ClientID: "test-app", Tenant: "consumers", OAuthFile: filepath.Join(t.TempDir(), "outlook.json")}
	if err := saveToken(a, &oauth2.Token{AccessToken: "old-access", RefreshToken: "old-refresh", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cfg := microsoftConfig(a)
	cfg.Endpoint.TokenURL = s.URL
	for range 2 {
		got, err := outlookToken(t.Context(), a, cfg)
		if err != nil || got != "new-access" {
			t.Fatalf("token = %q, %v", got, err)
		}
	}
	var state storedToken
	if err := decodeFile(a.OAuthFile, &state); err != nil || state.Token.RefreshToken != "rotated-refresh" || calls.Load() != 1 {
		t.Fatalf("rotated token was not persisted (calls %d, error %v)", calls.Load(), err)
	}
	info, err := os.Stat(a.OAuthFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected token file permissions: %v, %v", info, err)
	}
	a.Username = "other@example.net"
	if _, err := outlookToken(t.Context(), a, cfg); err == nil || calls.Load() != 1 {
		t.Fatal("wrong account must not reuse token")
	}
}

func TestXOAUTH2(t *testing.T) {
	mechanism, value, err := (xoauth2{"user@example.net", "access"}).Start()
	if err != nil || mechanism != "XOAUTH2" || string(value) != "user=user@example.net\x01auth=Bearer access\x01\x01" {
		t.Fatalf("mechanism = %q, value = %q, error = %v", mechanism, value, err)
	}
}
