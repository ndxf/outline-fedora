package keystore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSSA = "ss://YWVzLTI1Ni1nY206dGVzdHBhc3N3b3JkQQ==@1.2.3.4:8388#Home%20Server"
const testSSB = "ss://YWVzLTI1Ni1nY206dGVzdHBhc3N3b3JkQg==@5.6.7.8:8388#Office"
const testSSC = "ss://YWVzLTI1Ni1nY206dGVzdHBhc3N3b3JkQw==@9.10.11.12:8388"

func newStore(t *testing.T) *Store {
	t.Helper()
	p := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestOpenCreatesEmpty(t *testing.T) {
	s := newStore(t)
	if got := s.List(); len(got) != 0 {
		t.Errorf("expected empty list, got %d entries", len(got))
	}
	if s.Active() != "" {
		t.Errorf("expected no active, got %q", s.Active())
	}
}

func TestAddFirstBecomesActive(t *testing.T) {
	s := newStore(t)
	k, err := s.Add(testSSA, "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if k.Name != "home-server" {
		t.Errorf("derived name: got %q want %q", k.Name, "home-server")
	}
	if k.LabelFromURL != "Home Server" {
		t.Errorf("label: got %q want %q", k.LabelFromURL, "Home Server")
	}
	if s.Active() != "home-server" {
		t.Errorf("first key should be active, got %q", s.Active())
	}
}

func TestAddSecondDoesNotBecomeActive(t *testing.T) {
	s := newStore(t)
	if _, err := s.Add(testSSA, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(testSSB, ""); err != nil {
		t.Fatal(err)
	}
	if s.Active() != "home-server" {
		t.Errorf("active should stay on first key, got %q", s.Active())
	}
}

func TestAddSuggestedName(t *testing.T) {
	s := newStore(t)
	k, err := s.Add(testSSA, "my-home")
	if err != nil {
		t.Fatal(err)
	}
	if k.Name != "my-home" {
		t.Errorf("suggested name ignored: got %q", k.Name)
	}
}

func TestAddRejectsDuplicateURL(t *testing.T) {
	s := newStore(t)
	if _, err := s.Add(testSSA, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(testSSA, "b"); err == nil {
		t.Fatal("expected error on duplicate url")
	}
}

func TestAddRejectsDuplicateName(t *testing.T) {
	s := newStore(t)
	if _, err := s.Add(testSSA, "same"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(testSSB, "same"); err == nil {
		t.Fatal("expected error on duplicate name")
	}
}

func TestAddRejectsNonSSURL(t *testing.T) {
	s := newStore(t)
	if _, err := s.Add("https://example.com", ""); err == nil {
		t.Fatal("expected error on non-ss url")
	}
}

func TestAddNamelessFallback(t *testing.T) {
	s := newStore(t)
	// URL has no fragment — falls back to "key" derived name.
	k, err := s.Add(testSSC, "")
	if err != nil {
		t.Fatal(err)
	}
	if k.Name != "key" {
		t.Errorf("fallback name: got %q want %q", k.Name, "key")
	}
}

func TestRemove(t *testing.T) {
	s := newStore(t)
	s.Add(testSSA, "a")
	s.Add(testSSB, "b")
	if err := s.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := s.Get("a"); ok {
		t.Error("a still present after remove")
	}
	if s.Active() != "" {
		t.Errorf("active should clear when active key removed, got %q", s.Active())
	}
}

func TestRemoveMissing(t *testing.T) {
	s := newStore(t)
	if err := s.Remove("missing"); err == nil {
		t.Error("expected error removing missing key")
	}
}

func TestRename(t *testing.T) {
	s := newStore(t)
	s.Add(testSSA, "a")
	if err := s.Rename("a", "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("a"); ok {
		t.Error("old name still present")
	}
	if _, ok := s.Get("renamed"); !ok {
		t.Error("new name not present")
	}
	if s.Active() != "renamed" {
		t.Errorf("active pointer didn't follow rename: %q", s.Active())
	}
}

func TestRenameCollision(t *testing.T) {
	s := newStore(t)
	s.Add(testSSA, "a")
	s.Add(testSSB, "b")
	if err := s.Rename("a", "b"); err == nil {
		t.Error("expected collision error")
	}
}

func TestUse(t *testing.T) {
	s := newStore(t)
	s.Add(testSSA, "a")
	s.Add(testSSB, "b")
	if err := s.Use("b"); err != nil {
		t.Fatal(err)
	}
	if s.Active() != "b" {
		t.Errorf("active should be b, got %q", s.Active())
	}
}

func TestMarkConnectedUpdatesActiveAndTS(t *testing.T) {
	s := newStore(t)
	s.Add(testSSA, "a")
	s.Add(testSSB, "b")
	if err := s.MarkConnected("b"); err != nil {
		t.Fatal(err)
	}
	if s.Active() != "b" {
		t.Errorf("MarkConnected did not update active: %q", s.Active())
	}
	if s.state.LastConnectedAt.IsZero() {
		t.Error("LastConnectedAt not set")
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys.json")
	s1, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s1.Add(testSSA, "a")
	s1.Add(testSSB, "b")
	s1.Use("b")

	s2, err := Open(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(s2.List()) != 2 {
		t.Errorf("expected 2 keys after reopen, got %d", len(s2.List()))
	}
	if s2.Active() != "b" {
		t.Errorf("active not persisted, got %q", s2.Active())
	}
}

func TestFilePermissionsAre0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Add(testSSA, "a")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perms: got %o want 0600", fi.Mode().Perm())
	}
}

// Empty suggestedName is treated as "auto-derive" — not rejected.
// Anything non-empty must satisfy nameOK.
func TestInvalidNameRejected(t *testing.T) {
	s := newStore(t)
	for _, bad := range []string{"-nope", "UPPER", "with space", "with/slash", strings.Repeat("x", 33)} {
		if _, err := s.Add(testSSA, bad); err == nil {
			t.Errorf("expected rejection of name %q", bad)
		}
	}
}

func TestDeriveNameCollisionResolves(t *testing.T) {
	s := newStore(t)
	// Both have the same "home-server" fragment.
	a := "ss://YWVzLTI1Ni1nY206Ymxhbmsx@1.1.1.1:8388#home-server"
	b := "ss://YWVzLTI1Ni1nY206Ymxhbmsy@2.2.2.2:8388#home-server"
	ka, err := s.Add(a, "")
	if err != nil {
		t.Fatal(err)
	}
	kb, err := s.Add(b, "")
	if err != nil {
		t.Fatal(err)
	}
	if ka.Name == kb.Name {
		t.Errorf("expected distinct names, both got %q", ka.Name)
	}
}

func TestListSortedByAddedAt(t *testing.T) {
	s := newStore(t)
	s.Add(testSSA, "a")
	s.Add(testSSB, "b")
	s.Add(testSSC, "c")
	list := s.List()
	if list[0].Name != "a" || list[1].Name != "b" || list[2].Name != "c" {
		t.Errorf("list not sorted by added_at: %+v", list)
	}
}
