package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/lml2468/octobuddy/core/config"
)

// mediaHTTPClient is the media downloader's transport.
//
// - redirect: manual — we walk the chain ourselves so each hop is
// SSRF-revalidated and the Authorization header is recomputed per hop
// (fetchWithRedirectGuard parity).
// - DialControl: the actual socket address chosen by the resolver is
// re-checked against the private/local ranges at *dial time*. This closes the
// DNS-rebinding TOCTOU: AssertPublicURL resolves once for the policy check,
// but the transport resolves again to dial — a hostile authoritative DNS
// could return a public IP to the first lookup and 169.254.169.254 / a
// private IP to the second. Validating in Control (which runs on the exact
// address being connected) makes the check authoritative for the connection.
// - explicit Transport timeouts + per-host conn cap so a slow/hostile endpoint
// can't tie up connections (the ctx deadline still bounds the whole fetch).
var mediaHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   downloadTimeout,
			KeepAlive: 30 * time.Second,
			Control:   dialControlGuard,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxConnsPerHost:       8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: downloadTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// dialControlGuard rejects a connection whose resolved destination address is in
// a private/loopback/link-local/CGN/unspecified range. Runs after DNS resolution
// on the concrete address the kernel is about to connect to, so it defeats DNS
// rebinding that AssertPublicURL's earlier lookup cannot (the resolver may return
// a different IP at dial time).
func dialControlGuard(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("media dial: bad address %q: %w", address, err)
	}
	if config.IsPrivateOrLocalAddress(host) {
		return fmt.Errorf("media dial: refusing private/local address %s", host)
	}
	return nil
}

// fetchGuarded performs an SSRF-guarded GET, manually walking redirects so each
// hop is re-validated and the Authorization header is recomputed per hop
// (fetchWithRedirectGuard + assertPublicUrl parity).
func (g *Gateway) fetchGuarded(ctx context.Context, rawURL string) (*http.Response, error) {
	assertPublic := g.assertPublic
	if assertPublic == nil {
		assertPublic = config.AssertPublicURL
	}
	client := g.mediaClient
	if client == nil {
		client = mediaHTTPClient
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	current := rawURL
	for hop := 0; hop <= maxRedirects; hop++ {
		if err := assertPublic(ctx, current); err != nil {
			cancel()
			return nil, err
		}
		resp, err := g.fetchMediaHop(ctx, client, current)
		if err != nil {
			cancel()
			return nil, err
		}
		if isRedirectStatus(resp.StatusCode) {
			next, err := nextRedirectURL(current, resp)
			_ = resp.Body.Close()
			if err != nil {
				cancel()
				return nil, err
			}
			current = next
			continue
		}
		// Terminal response — wrap the body so closing it also cancels the ctx.
		resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
	cancel()
	return nil, fmt.Errorf("too many redirects (started at %s)", rawURL)
}

func (g *Gateway) fetchMediaHop(ctx context.Context, client *http.Client, current string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
	if err != nil {
		return nil, err
	}
	if g.mediaAuth != nil {
		if h := g.mediaAuth(current); h != "" {
			req.Header.Set("Authorization", h)
		}
	}
	return client.Do(req)
}

func isRedirectStatus(status int) bool {
	return status >= 300 && status < 400
}

func nextRedirectURL(current string, resp *http.Response) (string, error) {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("redirect without Location")
	}
	base, err := url.Parse(current)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(loc)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

// cancelOnCloseBody cancels the download context when the body is closed, so the
// per-download timeout's resources are always released.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
