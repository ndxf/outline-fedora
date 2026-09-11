//go:build linux

package tun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"

	"golang.getoutline.org/sdk/network"
	"golang.getoutline.org/sdk/network/lwip2transport"
	"golang.getoutline.org/sdk/x/configurl"

	"github.com/ndxf/outline-fedora/internal/snapshot"
)

// Session owns exactly one active VPN connection. All host state that
// gets mutated during Start() is recorded in a snapshot first, so a
// crash between mutations is recoverable by ouf-panic.
type Session struct {
	cfg  Config
	snap *snapshot.Writer

	// Populated during Start.
	tun     *tunDevice
	device  network.IPDevice
	svrIP   net.IP
	pumpsWG sync.WaitGroup

	// Cancellation.
	stopOnce sync.Once
	closed   chan struct{}
}

// New builds a Session and validates configuration. It does NOT touch
// host state — call Start for that.
func New(cfg Config) (*Session, error) {
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Session{
		cfg:    cfg,
		closed: make(chan struct{}),
	}, nil
}

// Start brings the session up. Order is fixed so revert is deterministic:
//
//  1. Write initial snapshot (marks + empty actions) so panic has a
//     minimum viable file even if we crash immediately.
//  2. Snapshot pre-state for IPv6 sysctl + resolv.conf.
//  3. Disable IPv6, resolve ss:// server IPv4 (must be done with IPv6
//     off so getaddrinfo doesn't hand back a v6 result).
//  4. Build the SDK OutlineDevice (Shadowsocks TCP + UDP through lwIP).
//  5. Create the TUN device.
//  6. Overwrite resolv.conf.
//  7. Install routes and ip rule.
//  8. Spawn tun<->device copy goroutines.
//
// Any error before step 8 rolls the whole session back via cleanup().
// After step 8 the session is running; the caller Wait()s for shutdown.
func (s *Session) Start(ctx context.Context) (err error) {
	// Refuse to start if a snapshot already exists — that means a prior
	// session died dirty and hasn't been cleaned up. Caller must run
	// panic first.
	snapPath := s.cfg.StateDir + "/" + snapshot.SnapshotFile
	if _, statErr := os.Stat(snapPath); statErr == nil {
		return fmt.Errorf("existing snapshot at %s — run ouf-panic before starting a new session", snapPath)
	}

	marks := snapshot.OurMarks{
		TunIfname:    s.cfg.TunName,
		TableID:      s.cfg.RoutingTableID,
		RulePriority: s.cfg.RoutingRulePriority,
	}
	s.snap, err = snapshot.New(s.cfg.StateDir, marks, s.cfg.OUFVersion)
	if err != nil {
		return fmt.Errorf("init snapshot: %w", err)
	}

	// Snapshot pre-state BEFORE any mutation.
	if err := s.snap.RecordResolvConf(s.cfg.ResolvPath); err != nil {
		return fmt.Errorf("record resolv.conf: %w", err)
	}
	ipv6Before, err := readIPv6Disabled()
	if err != nil {
		return fmt.Errorf("read ipv6 sysctl: %w", err)
	}
	s.snap.RecordSysctl(IPv6DisableSysctlKey, ipv6Before)

	// Check the tun ifname is not already taken.
	if _, e := net.InterfaceByName(s.cfg.TunName); e == nil {
		s.snap.SetTunExisted(true)
		return fmt.Errorf("tun %q already exists — refusing to co-opt an existing interface", s.cfg.TunName)
	}

	// Persist the pre-state snapshot before any mutation.
	if err := s.snap.Persist(); err != nil {
		return fmt.Errorf("persist initial snapshot: %w", err)
	}

	// From here on, any error must call s.cleanupOnFailure so the panic
	// script isn't left holding a snapshot that mostly-does-nothing.
	defer func() {
		if err != nil {
			s.cleanupOnFailure()
		}
	}()

	// 3. Disable IPv6, then resolve.
	if err = writeIPv6Disabled("1"); err != nil {
		return fmt.Errorf("disable ipv6: %w", err)
	}
	s.snap.AppendAction(snapshot.SysctlSetAction(IPv6DisableSysctlKey, "1", true))
	if err = s.snap.Persist(); err != nil {
		return err
	}

	s.svrIP, err = resolveShadowsocksServerIPv4(s.cfg.TransportURL)
	if err != nil {
		return fmt.Errorf("resolve ss server: %w", err)
	}

	// 4. Build OutlineDevice.
	sd, err := configurl.NewDefaultProviders().NewStreamDialer(ctx, s.cfg.TransportURL)
	if err != nil {
		return fmt.Errorf("build stream dialer: %w", err)
	}
	pp, err := newPacketProxy(ctx, s.cfg.TransportURL)
	if err != nil {
		return fmt.Errorf("build packet proxy: %w", err)
	}
	s.device, err = lwip2transport.ConfigureDevice(sd, pp)
	if err != nil {
		return fmt.Errorf("configure lwip: %w", err)
	}

	// 5. Create TUN.
	if s.tun, err = newTunDevice(s.cfg.TunName, s.cfg.TunIP); err != nil {
		return fmt.Errorf("create tun: %w", err)
	}
	s.snap.AppendAction(snapshot.TunCreateAction(s.cfg.TunName, true))
	if err = s.snap.Persist(); err != nil {
		return err
	}

	// 6. Overwrite resolv.conf.
	if err = s.writeResolvConf(); err != nil {
		return fmt.Errorf("write resolv.conf: %w", err)
	}

	// 7. Install routes + rule.
	if err = s.installRouting(); err != nil {
		return fmt.Errorf("install routing: %w", err)
	}

	// 8. Spawn traffic pumps.
	s.pumpsWG.Add(2)
	go func() {
		defer s.pumpsWG.Done()
		_, _ = io.Copy(s.device, s.tun)
	}()
	go func() {
		defer s.pumpsWG.Done()
		_, _ = io.Copy(s.tun, s.device)
	}()

	return nil
}

// Wait blocks until Close is called (or the pumps exit on their own,
// which happens when the tun or device is closed).
func (s *Session) Wait() {
	<-s.closed
	s.pumpsWG.Wait()
}

// Close tears the session down cleanly and returns only after ouf-panic
// (or its Go equivalent) has fully reverted host state. Idempotent.
func (s *Session) Close() error {
	var runErr error
	s.stopOnce.Do(func() {
		// Closing the tun and the device makes io.Copy return, which
		// releases the pumps.
		if s.tun != nil {
			_ = s.tun.Close()
		}
		if s.device != nil {
			_ = s.device.Close()
		}
		close(s.closed)
		s.pumpsWG.Wait()

		// Revert host state via the same panic script the emergency
		// path uses — one code path, tested with the netns harness.
		runErr = runPanicScript(s.cfg.StateDir, s.cfg.ResolvPath)
	})
	return runErr
}

// ServerIP returns the resolved IPv4 of the Shadowsocks server. Only
// meaningful after Start returns nil.
func (s *Session) ServerIP() net.IP { return s.svrIP }

// cleanupOnFailure is called when Start returns an error partway
// through. It closes anything that was opened and runs the panic script
// against whatever snapshot state made it to disk.
func (s *Session) cleanupOnFailure() {
	if s.tun != nil {
		_ = s.tun.Close()
	}
	if s.device != nil {
		_ = s.device.Close()
	}
	// Best-effort revert. If this fails, the snapshot stays on disk and
	// the user (or systemd's ExecStopPost) can run ouf-panic manually.
	_ = runPanicScript(s.cfg.StateDir, s.cfg.ResolvPath)
}

// resolveShadowsocksServerIPv4 mirrors upstream's resolver: parse the
// ss:// URL, DNS-resolve the hostname, pick the first IPv4. Errors on
// IPv6-only servers (matching upstream limitation, documented).
func resolveShadowsocksServerIPv4(transportConfig string) (net.IP, error) {
	if strings.Contains(transportConfig, "|") {
		return nil, errors.New("multi-part transport configs are not supported yet")
	}
	transportConfig = strings.TrimSpace(transportConfig)
	if transportConfig == "" {
		return nil, errors.New("transport url required")
	}
	u, err := url.Parse(transportConfig)
	if err != nil {
		return nil, fmt.Errorf("parse ss url: %w", err)
	}
	if u.Scheme != "ss" {
		return nil, fmt.Errorf("scheme must be ss, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("ss url has no host")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4, nil
		}
	}
	return nil, errors.New("IPv6-only Shadowsocks servers are not supported yet")
}
