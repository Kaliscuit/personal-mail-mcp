package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxHTTPBody        = 4 << 20
	maxHTTPConnections = 64
)

func mcpHandler(server *mcp.Server, token [32]byte, additionalAllowedHosts []string) http.Handler {
	allowed := make(map[string]struct{}, len(additionalAllowedHosts))
	for _, host := range additionalAllowedHosts {
		allowed[host] = struct{}{}
	}
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxHTTPBody,
	})
	mux := http.NewServeMux()
	slots := make(chan struct{}, 4)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		scheme, candidate, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		hash := sha256.Sum256([]byte(candidate))
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(hash[:], token[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Server busy", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHTTPBody))
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
				} else {
					http.Error(w, "Could not read request", http.StatusBadRequest)
				}
				return
			}
			if bytes.HasPrefix(bytes.TrimSpace(body), []byte{'['}) {
				http.Error(w, "JSON-RPC batch requests are not supported", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		upstream := r.WithContext(ctx)
		if isAdditionalLoopbackHost(r, allowed) {
			upstream.Host = "localhost"
		}
		transport.ServeHTTP(w, upstream)
	})
	return http.NewCrossOriginProtection().Handler(mux)
}

func isAdditionalLoopbackHost(r *http.Request, allowed map[string]struct{}) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if len(allowed) == 0 || !ok || local == nil || !local.IP.IsLoopback() {
		return false
	}
	host := r.Host
	if strings.Contains(host, ":") {
		var port string
		var err error
		host, port, err = net.SplitHostPort(host)
		n, portErr := strconv.ParseUint(port, 10, 16)
		if err != nil || portErr != nil || n == 0 || strings.HasPrefix(r.Host, "[") {
			return false
		}
	}
	if !validDNSHostname(host) {
		return false
	}
	_, ok = allowed[strings.ToLower(host)]
	return ok
}

type cappedListener struct {
	net.Listener
	slots chan struct{}
}

type countedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func (l cappedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &countedConn{Conn: c, release: func() { <-l.slots }}, nil
		default:
			_ = c.Close()
		}
	}
}

func serve(ctx context.Context, endpoints []string, handler http.Handler) error {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	var listeners []net.Listener
	slots := make(chan struct{}, maxHTTPConnections)
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()
	for _, endpoint := range endpoints {
		ln, err := net.Listen("tcp", endpoint)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", endpoint, err)
		}
		listeners = append(listeners, cappedListener{Listener: ln, slots: slots})
	}
	results := make(chan error, len(listeners))
	for _, ln := range listeners {
		log.Printf("listening at http://%s/mcp", ln.Addr())
		go func() { results <- srv.Serve(ln) }()
	}
	select {
	case err := <-results:
		_ = srv.Close()
		return err
	case <-ctx.Done():
		deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(deadline); err != nil {
			return srv.Close()
		}
		return nil
	}
}
