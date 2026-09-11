//go:build linux

package tun

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSessionStartAndCloseInNetns exercises the full mutation path
// (tun create + routes + rule + resolv.conf + ipv6 sysctl) against a
// fake ss:// URL whose server IPv4 resolves to 1.1.1.1 (which is
// routable but we never actually push traffic through the tunnel).
//
// After Start succeeds we assert host state contains our mutations,
// then Close and assert nothing is left.
//
// Must run inside `unshare -Umnr` so the netlink mutations don't touch
// the real host. See scripts/test-session-netns.sh which sets that up.
func TestSessionStartAndCloseInNetns(t *testing.T) {
	if os.Getenv("OUF_TEST_IN_NETNS") != "1" {
		t.Skip("skipping; re-run via scripts/test-session-netns.sh which unshares first")
	}

	dir := t.TempDir()
	resolvPath := filepath.Join(dir, "fake-resolv.conf")
	// Start with a plausible pre-state resolv.conf so RecordResolvConf
	// has something to back up.
	if err := os.WriteFile(resolvPath, []byte("nameserver 192.168.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake ss:// URL pointing to 1.1.1.1. aes-256-gcm:pw base64'd.
	userinfo := base64.URLEncoding.EncodeToString([]byte("aes-256-gcm:testpassword"))
	ssURL := fmt.Sprintf("ss://%s@1.1.1.1:8388#e2e", userinfo)

	cfg := Config{
		TunName:             "ouftst1",
		TunIP:               "10.234.0.1",
		TunGatewayCIDR:      "10.234.0.2/32",
		RoutingTableID:      9991,
		RoutingRulePriority: 29991,
		DNSServerIP:         "9.9.9.9",
		TransportURL:        ssURL,
		ResolvPath:          resolvPath,
		StateDir:            dir,
		OUFVersion:          "test",
	}

	// Point the session at the panic script in our repo.
	panicPath := findPanicScript(t)
	t.Setenv("OUF_PANIC_SCRIPT", panicPath)
	// Re-eval package-level var (init already ran on package load).
	PanicScriptPath = panicPath

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Snapshot should exist now.
	if _, err := os.Stat(filepath.Join(dir, "snapshot.json")); err != nil {
		t.Errorf("snapshot missing after Start: %v", err)
	}
	// resolv.conf should now be our version.
	got, _ := os.ReadFile(resolvPath)
	if !strings.Contains(string(got), "9.9.9.9") {
		t.Errorf("resolv.conf not rewritten: %q", got)
	}

	// Close should revert everything.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Snapshot should be gone (panic script cleared it).
	if _, err := os.Stat(filepath.Join(dir, "snapshot.json")); !os.IsNotExist(err) {
		t.Errorf("snapshot not cleared after Close: %v", err)
	}
	// resolv.conf should be restored.
	got, _ = os.ReadFile(resolvPath)
	if string(got) != "nameserver 192.168.0.1\n" {
		t.Errorf("resolv.conf not restored: %q", got)
	}
}

func findPanicScript(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, "scripts", "ouf-panic")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate scripts/ouf-panic")
	return ""
}
