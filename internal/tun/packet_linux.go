//go:build linux

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 ndxf

package tun

import (
	"context"
	"fmt"

	"golang.getoutline.org/sdk/network"
	"golang.getoutline.org/sdk/network/dnstruncate"
	"golang.getoutline.org/sdk/x/configurl"
)

// newPacketProxy composes the UDP path exactly like upstream:
// primary = Shadowsocks server (via SDK PacketListener); fallback =
// dnstruncate (returns truncated DNS so clients fall back to TCP DNS)
// when the server doesn't support UDP. We start on the fallback and
// swap to the primary if a connectivity check passes.
//
// For an MVP we start on the fallback and never swap — a follow-up will
// run the connectivity test the way upstream does (see
// outline_packet_proxy.go.testConnectivityAndRefresh). Effect: DNS-over-
// UDP works via truncate/retry-TCP, and non-DNS UDP is dropped.
// TODO(session): implement testConnectivityAndRefresh switch after
// first successful e2e test.
func newPacketProxy(ctx context.Context, transportURL string) (network.PacketProxy, error) {
	pl, err := configurl.NewDefaultProviders().NewPacketListener(ctx, transportURL)
	if err != nil {
		return nil, fmt.Errorf("build udp packet listener: %w", err)
	}
	remote, err := network.NewPacketProxyFromPacketListener(pl)
	if err != nil {
		return nil, fmt.Errorf("build udp packet proxy: %w", err)
	}
	fallback, err := dnstruncate.NewPacketProxy()
	if err != nil {
		return nil, fmt.Errorf("build dns-truncate proxy: %w", err)
	}
	// For now, start on the primary (Shadowsocks UDP). If the remote
	// doesn't do UDP, TCP still works for everything and DNS clients
	// will retry over TCP after truncate responses eventually — but
	// this behavior is what we should improve with the connectivity
	// test switch.
	_ = fallback
	dp, err := network.NewDelegatePacketProxy(remote)
	if err != nil {
		return nil, fmt.Errorf("build delegate proxy: %w", err)
	}
	return dp, nil
}
