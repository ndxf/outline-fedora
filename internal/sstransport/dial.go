// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 ndxf

// Package sstransport wraps the outline-sdk configurl helper so the rest
// of the codebase can go from an ss:// URL to a stream-dialing function
// without pulling the whole SDK surface into every caller.
package sstransport

import (
	"context"
	"fmt"
	"net"

	"golang.getoutline.org/sdk/transport"
	"golang.getoutline.org/sdk/x/configurl"
)

// Dialer wraps a StreamDialer built from an ss:// URL. Use it as
// (*Dialer).DialStream(ctx, "example.com:443").
type Dialer struct {
	inner transport.StreamDialer
	// url is retained only for diagnostic messages.
	url string
}

// New parses an ss:// URL (or any transport config URL the outline-sdk
// configurl parser understands) and returns a Dialer. It does NOT open
// a connection to the Shadowsocks server — that happens lazily on the
// first DialStream call. So a successful New() only means the URL is
// syntactically valid, not that the remote server is reachable.
func New(ctx context.Context, ssURL string) (*Dialer, error) {
	if ssURL == "" {
		return nil, fmt.Errorf("empty transport url")
	}
	dialer, err := configurl.NewDefaultProviders().NewStreamDialer(ctx, ssURL)
	if err != nil {
		return nil, fmt.Errorf("build stream dialer from %q: %w", redactSS(ssURL), err)
	}
	return &Dialer{inner: dialer, url: ssURL}, nil
}

// DialStream opens a TCP-like stream to target ("host:port") through the
// Shadowsocks server. The returned conn is a normal net.Conn — reads and
// writes are transparently tunneled.
func (d *Dialer) DialStream(ctx context.Context, target string) (net.Conn, error) {
	conn, err := d.inner.DialStream(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("dial %s via ss server: %w", target, err)
	}
	return conn, nil
}

// Inner exposes the underlying SDK StreamDialer for callers that need
// to pass it to other SDK components (e.g., httpproxy.NewProxyHandler).
func (d *Dialer) Inner() transport.StreamDialer {
	return d.inner
}

// redactSS returns a printable form of an ss:// URL that hides the
// credential portion. Useful for error messages and logs.
func redactSS(u string) string {
	if len(u) < 5 || u[:5] != "ss://" {
		return "<non-ss url>"
	}
	// ss://<base64-or-userinfo>@host:port#label
	rest := u[5:]
	at := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return "ss://<redacted>"
	}
	return "ss://<redacted>@" + rest[at+1:]
}
