package main

import (
	"crypto/sha256"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPHTTP(t *testing.T) {
	const token = "secret-for-tests-only-at-least-32-chars"
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	h := mcpHandler(s, sha256.Sum256([]byte(token)), nil)
	for _, tc := range []struct {
		name, auth, fetchSite string
		status                int
	}{
		{"missing", "", "", 401}, {"wrong", "Bearer wrong", "", 401},
		{"cross-site", "Bearer " + token, "cross-site", 403},
		{"authorized", "Bearer " + token, "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`))
			r.Header.Set("Authorization", tc.auth)
			r.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			r.Header.Set("Accept", "application/json, text/event-stream")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
		})
	}
}

func TestAdditionalAllowedHosts(t *testing.T) {
	const token = "secret-for-tests-only-at-least-32-chars"
	const publicHost = "mail.example.test"
	const init = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	hash := sha256.Sum256([]byte(token))
	configured := httptest.NewServer(mcpHandler(s, hash, []string{publicHost}))
	defer configured.Close()
	defaults := httptest.NewServer(mcpHandler(s, hash, nil))
	defer defaults.Close()
	for _, tc := range []struct {
		name, host, auth, origin, forwardedHost string
		server                                  *httptest.Server
		status                                  int
	}{
		{"allowlisted", publicHost, token, "", "", configured, 200},
		{"case and port", "MAIL.EXAMPLE.TEST:443", token, "", "", configured, 200},
		{"same origin", publicHost, token, "https://" + publicHost, "", configured, 200},
		{"loopback remains valid", "localhost:18080", token, "", "", configured, 200},
		{"no configuration", publicHost, token, "", "", defaults, 403},
		{"unlisted", "other.example.test", token, "", "", configured, 403},
		{"subdomain", "sub." + publicHost, token, "", "", configured, 403},
		{"invalid port", publicHost + ":65536", token, "", "", configured, 403},
		{"trailing dot", publicHost + ".", token, "", "", configured, 403},
		{"forwarded host ignored", "other.example.test", token, "", publicHost, configured, 403},
		{"missing token", publicHost, "", "", "", configured, 401},
		{"wrong token", publicHost, "wrong", "", "", configured, 401},
		{"cross origin", publicHost, token, "https://other.example.test", "", configured, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, tc.server.URL+"/mcp", strings.NewReader(init))
			if err != nil {
				t.Fatal(err)
			}
			r.Host = tc.host
			if tc.auth != "" {
				r.Header.Set("Authorization", "Bearer "+tc.auth)
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			r.Header.Set("X-Forwarded-Host", tc.forwardedHost)
			r.Header.Set("Accept", "application/json, text/event-stream")
			r.Header.Set("Content-Type", "application/json")
			resp, err := tc.server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

type bearerTransport string

func (token bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(token))
	return http.DefaultTransport.RoundTrip(r)
}

func TestMCPToolCalls(t *testing.T) {
	const token = "secret-for-tests-only-at-least-32-chars"
	service := newMailService(config{Accounts: map[string]account{
		"personal": {Provider: "gmail", Username: "not-visible@example.net", PasswordFile: "secret-path"},
		"work":     {Provider: "outlook"},
	}})
	h := httptest.NewServer(mcpHandler(newServer(service), sha256.Sum256([]byte(token)), nil))
	defer h.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	conn, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: h.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport(token)}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tools, err := conn.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		readOnly := slices.Contains([]string{"list_accounts", "list_attachments", "list_folders", "read_message", "search_messages"}, tool.Name)
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != readOnly {
			t.Fatalf("tool %s has incorrect read-only annotation", tool.Name)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"archive_message", "delete_message", "list_accounts", "list_attachments", "list_folders", "move_message", "read_message", "search_messages", "set_message_read"}) {
		t.Fatalf("unexpected tools: %v", names)
	}
	result, err := conn.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_accounts"})
	if err != nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("list_accounts failed: %v, %v", result, err)
	}
	data := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(data, `"account":"personal"`) || strings.Contains(data, "secret-path") || strings.Contains(data, "not-visible") {
		t.Fatalf("unexpected list_accounts output: %s", data)
	}
	result, err = conn.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]string{"account": "absent"}})
	if err != nil || !result.IsError {
		t.Fatalf("unknown account must return a tool error: %v, %v", result, err)
	}
}

func TestHTTPRejectsBatchAndOversize(t *testing.T) {
	const token = "secret-for-tests-only-at-least-32-chars"
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	h := mcpHandler(s, sha256.Sum256([]byte(token)), nil)
	for _, tc := range []struct {
		body string
		want int
	}{{` [{"jsonrpc":"2.0","id":1,"method":"ping"}]`, 400}, {strings.Repeat(" ", maxHTTPBody+1), 413}} {
		r := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("got %d, want %d", w.Code, tc.want)
		}
	}
}

func TestConnectionSlotsAreReleased(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cap := cappedListener{Listener: ln, slots: make(chan struct{}, 1)}
	dial := func() net.Conn {
		c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	dial()
	first, err := cap.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := cap.Accept()
		accepted <- c
	}()
	overflow := dial()
	_ = overflow.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := overflow.Read(make([]byte, 1)); err == nil || os.IsTimeout(err) {
		t.Fatalf("overflow connection was not closed: %v", err)
	}
	_ = first.Close()
	_ = first.Close()
	dial()
	select {
	case c := <-accepted:
		if c == nil {
			t.Fatal("listener did not accept the next connection")
		}
		_ = c.Close()
	case <-time.After(time.Second):
		t.Fatal("slot did not become available after close")
	}
}

func TestListenerAddresses(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback == 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		endpoints, err := listenAddresses([]string{iface.Name, iface.Name}, 8080)
		if err != nil {
			t.Fatal(err)
		}
		seen := make(map[string]bool)
		for _, ep := range endpoints {
			host, port, err := net.SplitHostPort(ep)
			if err != nil || port != "8080" || !net.ParseIP(host).IsLoopback() || seen[ep] {
				t.Fatalf("unexpected listener %s", ep)
			}
			seen[ep] = true
		}
		return
	}
	t.Fatal("no active loopback interface")
}

func TestMissingInterface(t *testing.T) {
	if _, err := listenAddresses([]string{"mail-mcp-test-absent-interface"}, 8080); err == nil {
		t.Fatal("expected missing interface to fail")
	}
}
