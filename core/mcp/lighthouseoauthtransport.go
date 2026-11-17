package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/maximhq/bifrost/core/schemas"
)

// lighthouseOAuthTransport refreshes shared OAuth authorization on the existing
// HTTP connection. A credential lookup failure aborts before any network send;
// the bearer captured when connecting is never a fallback.
type lighthouseOAuthTransport struct {
	origin  *url.URL
	base    http.RoundTripper
	resolve func(context.Context) (http.Header, error)
}

func (t *lighthouseOAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	fail := func(err error) (*http.Response, error) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	if err := req.Context().Err(); err != nil {
		return fail(err)
	}
	if t.origin == nil || !strings.EqualFold(req.URL.Scheme, t.origin.Scheme) || !strings.EqualFold(req.URL.Host, t.origin.Host) {
		return fail(fmt.Errorf("refusing shared OAuth redirect outside configured origin"))
	}
	headers, err := t.resolve(req.Context())
	if err != nil {
		return fail(err)
	}
	if err := req.Context().Err(); err != nil {
		return fail(err)
	}
	authorization := headers.Get("Authorization")
	if strings.TrimSpace(authorization) == "" {
		return fail(fmt.Errorf("shared OAuth authorization unavailable"))
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", authorization)
	return t.base.RoundTrip(clone)
}
func (m *MCPManager) withLighthouseOAuthRefresh(httpClient *http.Client, config *schemas.MCPClientConfig, endpoint string) *http.Client {
	if config.AuthType != schemas.MCPAuthTypeOauth {
		return httpClient
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	copyClient := *httpClient
	base := copyClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	origin, _ := url.Parse(endpoint)
	copyClient.Transport = &lighthouseOAuthTransport{origin: origin, base: base, resolve: func(ctx context.Context) (http.Header, error) {
		return m.credStore.ConnectionHeaders(schemas.NewBifrostContext(ctx, schemas.NoDeadline), config)
	}}
	return &copyClient
}

func (t *lighthouseOAuthTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// lighthouseSharedHTTPTransport keeps SDK interfaces and releases the idle pool after session teardown.
type lighthouseSharedHTTPTransport struct {
	*transport.StreamableHTTP
	httpClient *http.Client
	closeOnce  sync.Once
	closeErr   error
}

func (t *lighthouseSharedHTTPTransport) Close() error {
	t.closeOnce.Do(func() { defer t.httpClient.CloseIdleConnections(); t.closeErr = t.StreamableHTTP.Close() })
	return t.closeErr
}

var _ transport.Interface = (*lighthouseSharedHTTPTransport)(nil)
var _ transport.HTTPConnection = (*lighthouseSharedHTTPTransport)(nil)
var _ transport.BidirectionalInterface = (*lighthouseSharedHTTPTransport)(nil)
