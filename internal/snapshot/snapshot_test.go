// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 ndxf

package snapshot

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWriterRoundTrip(t *testing.T) {
	dir := t.TempDir()

	w, err := New(dir, OurMarks{
		TunIfname:    "ouftest0",
		TableID:      9999,
		RulePriority: 29999,
	}, "test-0.0.0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w.RecordSysctl("net.ipv6.conf.lo.disable_ipv6", "0")
	w.AppendAction(TunCreateAction("ouftest0", true))
	w.AppendAction(RouteAddAction(9999, "default dev ouftest0", true))
	w.AppendAction(RuleAddAction(29999, 9999, "", true))
	w.AppendAction(SysctlSetAction("net.ipv6.conf.lo.disable_ipv6", "1", true))

	if err := w.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	// Parse it back with a plain map to catch schema drift.
	data, err := os.ReadFile(filepath.Join(dir, SnapshotFile))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if got := m["version"]; got != float64(1) {
		t.Errorf("version: got %v want 1", got)
	}

	marks := m["our_marks"].(map[string]any)
	if marks["tun_ifname"] != "ouftest0" {
		t.Errorf("tun_ifname wrong: %v", marks["tun_ifname"])
	}
	if marks["table_id"] != float64(9999) {
		t.Errorf("table_id wrong: %v", marks["table_id"])
	}

	acts := m["actions"].([]any)
	if len(acts) != 4 {
		t.Fatalf("actions: got %d want 4", len(acts))
	}
	for i, a := range acts {
		m := a.(map[string]any)
		if m["seq"] != float64(i+1) {
			t.Errorf("action %d seq: got %v want %d", i, m["seq"], i+1)
		}
	}
}

func TestRecordResolvConfRegularFile(t *testing.T) {
	dir := t.TempDir()
	resolv := filepath.Join(dir, "fake-resolv.conf")
	if err := os.WriteFile(resolv, []byte("nameserver 192.168.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := New(dir, OurMarks{TunIfname: "ouftest0", TableID: 9999, RulePriority: 29999}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.RecordResolvConf(resolv); err != nil {
		t.Fatalf("RecordResolvConf: %v", err)
	}

	rc := w.snapshot.PreState.ResolvConf
	if !rc.Existed {
		t.Error("Existed=false, want true")
	}
	if rc.WasSymlink {
		t.Error("WasSymlink=true, want false")
	}
	if rc.SHA256Before == "" {
		t.Error("SHA256Before empty")
	}
	if rc.BackupPath == "" {
		t.Error("BackupPath empty")
	}
	if _, err := os.Stat(rc.BackupPath); err != nil {
		t.Errorf("backup not written: %v", err)
	}
}

func TestRecordResolvConfSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "stub.conf")
	if err := os.WriteFile(target, []byte("nameserver 127.0.0.53\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolv := filepath.Join(dir, "resolv.conf")
	if err := os.Symlink(target, resolv); err != nil {
		t.Fatal(err)
	}

	w, err := New(dir, OurMarks{TunIfname: "ouftest0", TableID: 9999, RulePriority: 29999}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.RecordResolvConf(resolv); err != nil {
		t.Fatal(err)
	}

	rc := w.snapshot.PreState.ResolvConf
	if !rc.Existed || !rc.WasSymlink {
		t.Errorf("expected existed+symlink, got %+v", rc)
	}
	if rc.SymlinkTarget != target {
		t.Errorf("target: got %q want %q", rc.SymlinkTarget, target)
	}
}

func TestRecordResolvConfAbsent(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, OurMarks{TunIfname: "ouftest0", TableID: 9999, RulePriority: 29999}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.RecordResolvConf(filepath.Join(dir, "does-not-exist")); err != nil {
		t.Fatal(err)
	}
	rc := w.snapshot.PreState.ResolvConf
	if rc.Existed {
		t.Error("Existed=true for absent file")
	}
}

// TestWireCompatWithPanicScript writes a snapshot with the Go writer and
// then invokes the actual ouf-panic shell script (via unshare -Umnr) to
// prove they agree on the format. If this test passes, we can be
// confident the daemon's snapshots will actually be revertable.
func TestWireCompatWithPanicScript(t *testing.T) {
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare not available")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	panicScript := findPanicScript(t)

	dir := t.TempDir()

	// Fabricate a "before" resolv.conf and back it up.
	resolvPath := filepath.Join(dir, "fake-resolv.conf")
	origContent := []byte("nameserver 192.168.0.1\n")
	if err := os.WriteFile(resolvPath, origContent, 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := New(dir, OurMarks{
		TunIfname:    "ouftest9",
		TableID:      9991,
		RulePriority: 29991,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.RecordResolvConf(resolvPath); err != nil {
		t.Fatal(err)
	}
	w.AppendAction(ResolvWriteAction(resolvPath, "unused-in-test", true))

	if err := w.Persist(); err != nil {
		t.Fatal(err)
	}

	// Simulate the "mutated" state that connect would leave behind.
	if err := os.WriteFile(resolvPath, []byte("nameserver 9.9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Now run panic. It should read our snapshot, restore resolv.conf,
	// and clear the snapshot file.
	cmd := exec.Command("unshare", "-Umnr", "--propagation=private", "--",
		panicScript)
	cmd.Env = append(os.Environ(),
		"OUF_STATE_DIR="+dir,
		"OUF_RESOLV_PATH="+resolvPath,
		"OUF_VERBOSE=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("panic script failed: %v\n%s", err, out)
	}

	// Snapshot file should be gone.
	if _, err := os.Stat(filepath.Join(dir, SnapshotFile)); !os.IsNotExist(err) {
		t.Errorf("snapshot not cleared after successful revert: %v", err)
	}

	// Resolv file should be restored.
	got, err := os.ReadFile(resolvPath)
	if err != nil {
		t.Fatalf("read resolv after revert: %v", err)
	}
	if string(got) != string(origContent) {
		t.Errorf("resolv.conf not restored\n got: %q\nwant: %q\nscript output:\n%s",
			got, origContent, out)
	}
}

func findPanicScript(t *testing.T) string {
	t.Helper()
	// Test runs from the package dir; walk up to find scripts/ouf-panic.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "scripts", "ouf-panic")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate scripts/ouf-panic")
	return ""
}
