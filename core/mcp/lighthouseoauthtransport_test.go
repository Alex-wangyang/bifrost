package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	sdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

type lighthouseCredStore struct {
	schemas.MCPCredentialStore
	mu     sync.Mutex
	bearer string
	err    error
}

func (s *lighthouseCredStore) ConnectionHeaders(*schemas.BifrostContext, *schemas.MCPClientConfig) (http.Header, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return http.Header{"Authorization": []string{s.bearer}}, s.err
}
func TestLighthouseSharedHTTPRefreshAndFailure(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(202)
			return
		}
		var result any = map[string]any{}
		if req.Method == "initialize" {
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "1"}}
		}
		if req.Method == "tools/list" {
			result = map[string]any{"tools": []any{}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()
	store := &lighthouseCredStore{bearer: "Bearer old"}
	m := &MCPManager{credStore: store, logger: &MockLogger{}}
	config := &schemas.MCPClientConfig{ID: "fixture", Name: "fixture", ConnectionType: schemas.MCPConnectionTypeHTTP, ConnectionString: schemas.NewSecretVar(srv.URL), AuthType: schemas.MCPAuthTypeOauth, NeedsSessionStickiness: schemas.Ptr(true)}
	conn, _, err := m.createHTTPConnection(context.Background(), config, nil, "")
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.Start(context.Background()))
	init := sdk.InitializeRequest{}
	init.Params.ProtocolVersion = "2025-06-18"
	init.Params.ClientInfo = sdk.Implementation{Name: "fixture", Version: "1"}
	_, err = conn.Initialize(context.Background(), init)
	require.NoError(t, err)
	_, err = conn.ListTools(context.Background(), sdk.ListToolsRequest{})
	require.NoError(t, err)
	store.mu.Lock()
	store.bearer = "Bearer rotated"
	store.mu.Unlock()
	_, err = conn.ListTools(context.Background(), sdk.ListToolsRequest{})
	require.NoError(t, err)
	mu.Lock()
	require.Equal(t, "Bearer rotated", seen[len(seen)-1])
	count := len(seen)
	mu.Unlock()
	store.mu.Lock()
	store.err = errors.New("credential revoked")
	store.mu.Unlock()
	_, err = conn.ListTools(context.Background(), sdk.ListToolsRequest{})
	require.ErrorContains(t, err, "credential revoked")
	mu.Lock()
	require.Len(t, seen, count)
	mu.Unlock()
}

func TestLighthouseOAuthRedirectCannotLeakBearer(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(200) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer origin.Close()
	m := &MCPManager{credStore: &lighthouseCredStore{bearer: "Bearer private"}}
	c := m.withLighthouseOAuthRefresh(nil, &schemas.MCPClientConfig{AuthType: schemas.MCPAuthTypeOauth}, origin.URL)
	_, err := c.Get(origin.URL)
	require.ErrorContains(t, err, "outside configured origin")
	require.False(t, reached)
}

type lhCloseBody struct{ closed bool }

func (b *lhCloseBody) Read(p []byte) (int, error) { return 0, io.EOF }
func (b *lhCloseBody) Close() error               { b.closed = true; return nil }

type lhTransportProbe struct{ calls, closes int }

func (b *lhTransportProbe) RoundTrip(r *http.Request) (*http.Response, error) {
	b.calls++
	return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
}
func (b *lhTransportProbe) CloseIdleConnections() { b.closes++ }
func TestLighthouseOAuthLifecycle(t *testing.T) {
	for _, mode := range []string{"cancel-before", "cancel-during", "resolver-error", "empty", "cross-origin"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := &lhTransportProbe{}
			body := &lhCloseBody{}
			origin, _ := url.Parse("https://fixture.invalid")
			req, _ := http.NewRequestWithContext(ctx, "DELETE", origin.String(), body)
			resolve := func(context.Context) (http.Header, error) {
				if mode == "cancel-during" {
					cancel()
				}
				if mode == "resolver-error" {
					return nil, errors.New("revoked")
				}
				if mode == "empty" {
					return http.Header{}, nil
				}
				return http.Header{"Authorization": []string{"Bearer test"}}, nil
			}
			if mode == "cancel-before" {
				cancel()
			}
			if mode == "cross-origin" {
				req.URL.Host = "other.invalid"
			}
			tr := &lighthouseOAuthTransport{origin: origin, base: b, resolve: resolve}
			_, err := tr.RoundTrip(req)
			require.Error(t, err)
			require.Zero(t, b.calls)
			require.True(t, body.closed)
		})
	}
	b := &lhTransportProbe{}
	c := &http.Client{Transport: &lighthouseOAuthTransport{base: b}}
	c.CloseIdleConnections()
	require.Equal(t, 1, b.closes)
}

type lhDelegatingTransport struct {
	base   http.RoundTripper
	closes int
}

func (t *lhDelegatingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(r)
}
func (t *lhDelegatingTransport) CloseIdleConnections() {
	t.closes++
	t.base.(interface{ CloseIdleConnections() }).CloseIdleConnections()
}
func TestLighthouseOAuthSessionClose(t *testing.T) {
	var deletes int
	var deleteAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deletes++
			deleteAuth = r.Header.Get("Authorization")
			w.WriteHeader(200)
			return
		}
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var q struct {
			ID any `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		if q.ID == nil {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "fixture-session")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}}})
	}))
	defer srv.Close()
	origin, _ := url.Parse(srv.URL)
	base := &lhDelegatingTransport{base: http.DefaultTransport.(*http.Transport).Clone()}
	bearer := "Bearer first"
	hc := &http.Client{Transport: &lighthouseOAuthTransport{origin: origin, base: base, resolve: func(context.Context) (http.Header, error) { return http.Header{"Authorization": []string{bearer}}, nil }}}
	tr, err := transport.NewStreamableHTTP(srv.URL, transport.WithHTTPBasicClient(hc))
	require.NoError(t, err)
	wrapped := &lighthouseSharedHTTPTransport{StreamableHTTP: tr, httpClient: hc}
	conn := client.NewClient(wrapped)
	require.NoError(t, conn.Start(context.Background()))
	q := sdk.InitializeRequest{}
	q.Params.ProtocolVersion = "2025-06-18"
	q.Params.ClientInfo = sdk.Implementation{Name: "fixture", Version: "1"}
	_, err = conn.Initialize(context.Background(), q)
	require.NoError(t, err)
	bearer = "Bearer rotated"
	require.NoError(t, conn.Close())
	require.NoError(t, conn.Close())
	require.Equal(t, 1, deletes)
	require.Equal(t, "Bearer rotated", deleteAuth)
	require.Equal(t, 1, base.closes)
}
