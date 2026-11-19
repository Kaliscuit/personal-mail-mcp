package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

func microsoftConfig(a account) oauth2.Config {
	base := "https://login.microsoftonline.com/" + a.Tenant + "/oauth2/v2.0/"
	return oauth2.Config{ClientID: a.ClientID, Endpoint: oauth2.Endpoint{
		TokenURL: base + "token", DeviceAuthURL: base + "devicecode", AuthStyle: oauth2.AuthStyleInParams,
	}, Scopes: []string{"https://outlook.office.com/IMAP.AccessAsUser.All", "offline_access"}}
}

func oauthContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{
		Timeout:       12 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	})
}

type storedToken struct {
	Username string       `json:"username"`
	ClientID string       `json:"client_id"`
	Tenant   string       `json:"tenant"`
	Token    oauth2.Token `json:"token"`
}

func saveToken(a account, token *oauth2.Token) error {
	if token.RefreshToken == "" || token.AccessToken == "" || token.Expiry.IsZero() {
		return errors.New("Microsoft did not return a renewable token")
	}
	f, err := os.CreateTemp(filepath.Dir(a.OAuthFile), ".mcp-token-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := json.NewEncoder(f).Encode(storedToken{a.Username, a.ClientID, a.Tenant, *token}); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), a.OAuthFile)
}

func authorizeOutlook(ctx context.Context, a account) error {
	ctx, cancel := context.WithTimeout(oauthContext(ctx), 15*time.Minute)
	defer cancel()
	cfg := microsoftConfig(a)
	device, err := cfg.DeviceAuth(ctx)
	if err != nil {
		return errors.New("Microsoft device authorization could not start; check app ID, tenant and public client settings")
	}
	fmt.Printf("Visit %s and enter code %s. Sign in as %s.\n", device.VerificationURI, device.UserCode, a.Username)
	token, err := cfg.DeviceAccessToken(ctx, device)
	if err != nil {
		return errors.New("Microsoft device authorization did not complete")
	}
	return saveToken(a, token)
}

func outlookToken(ctx context.Context, a account, cfg oauth2.Config) (string, error) {
	var stored storedToken
	if err := decodeFile(a.OAuthFile, &stored); err != nil || stored.Username != a.Username || stored.ClientID != a.ClientID || stored.Tenant != a.Tenant || stored.Token.RefreshToken == "" || stored.Token.Expiry.IsZero() {
		return "", errors.New("Outlook authorization unavailable; run authorize-outlook on the server")
	}
	token, err := cfg.TokenSource(oauthContext(ctx), &stored.Token).Token()
	if err != nil || token.AccessToken == "" {
		return "", errors.New("Outlook authorization could not be refreshed; check connectivity or reauthorize on the server")
	}
	if token.AccessToken != stored.Token.AccessToken || token.RefreshToken != stored.Token.RefreshToken || !token.Expiry.Equal(stored.Token.Expiry) {
		if err := saveToken(a, token); err != nil {
			return "", errors.New("could not persist renewed Outlook authorization")
		}
	}
	return token.AccessToken, nil
}

type xoauth2 struct{ username, token string }

func (x xoauth2) Start() (string, []byte, error) {
	if strings.ContainsAny(x.username+x.token, "\r\n\x00\x01") {
		return "", nil, errors.New("invalid OAuth credentials")
	}
	return "XOAUTH2", []byte("user=" + x.username + "\x01auth=Bearer " + x.token + "\x01\x01"), nil
}

func (xoauth2) Next([]byte) ([]byte, error) { return []byte{}, nil }
