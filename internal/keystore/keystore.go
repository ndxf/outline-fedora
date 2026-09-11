// Package keystore manages the persistent list of ss:// keys and which
// one is the "active" (default) target. Storage is a single JSON file
// with an advisory flock for concurrent safety.
//
// This package deliberately knows nothing about networking. It's a
// dumb, well-tested container that the daemon consults on connect and
// mutates on add/remove/rename/use.
package keystore

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	Version         = 1
	DefaultPath     = "/etc/outline-fedora/keys.json"
	maxNameLen      = 32
	maxKeys         = 128
)

// Store is the in-memory + on-disk representation of the key list. All
// public methods take an internal mutex; a file-level flock is taken
// around read/write for cross-process safety.
type Store struct {
	path string

	mu    sync.Mutex
	state fileState
}

type fileState struct {
	Version         int       `json:"version"`
	Keys            []Key     `json:"keys"`
	Active          string    `json:"active,omitempty"`
	LastConnectedAt time.Time `json:"last_connected_at,omitempty"`
}

// Key is one stored ss:// entry.
type Key struct {
	Name         string    `json:"name"`
	URL          string    `json:"url"`
	LabelFromURL string    `json:"label_from_url,omitempty"`
	AddedAt      time.Time `json:"added_at"`
}

// Open loads (or initialises) a store at path. If the file doesn't
// exist it is created with an empty key list. Parent directory is
// created 0o755 if missing.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir keystore dir: %w", err)
	}
	s := &Store{path: path}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// reload reads the file (with shared flock) into memory, or initialises
// empty state if the file is missing.
func (s *Store) reload() error {
	f, err := os.OpenFile(s.path, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.state = fileState{Version: Version, Keys: []Key{}}
			return nil
		}
		return fmt.Errorf("open keystore: %w", err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return fmt.Errorf("flock keystore (shared): %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var st fileState
	if err := dec.Decode(&st); err != nil {
		return fmt.Errorf("decode keystore: %w", err)
	}
	if st.Version != Version {
		return fmt.Errorf("keystore version %d unsupported (this build handles %d)", st.Version, Version)
	}
	if st.Keys == nil {
		st.Keys = []Key{}
	}
	s.state = st
	return nil
}

// persist writes state atomically with an exclusive flock. Caller must
// hold s.mu.
func (s *Store) persist() error {
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".keys.*.tmp")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpPath) }

	if err := syscall.Flock(int(tmp.Fd()), syscall.LOCK_EX); err != nil {
		cleanup()
		return fmt.Errorf("flock tempfile: %w", err)
	}

	data, err := json.MarshalIndent(&s.state, "", "  ")
	if err != nil {
		cleanup()
		return fmt.Errorf("marshal: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write tempfile: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync tempfile: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	syscall.Flock(int(tmp.Fd()), syscall.LOCK_UN)
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close tempfile: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// List returns a sorted copy of all keys. Sort is by AddedAt ascending
// so the "first added" key is first — stable for humans.
func (s *Store) List() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Key, len(s.state.Keys))
	copy(out, s.state.Keys)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].AddedAt.Before(out[j].AddedAt)
	})
	return out
}

// Active returns the name of the currently-active key, or "" if none.
func (s *Store) Active() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Active
}

// Get returns the key with the given name, or (Key{}, false).
func (s *Store) Get(name string) (Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.state.Keys {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}

// Add stores a new key. If suggestedName is empty, a name is derived
// from the ss:// URL fragment or auto-numbered. If the name collides,
// an error is returned — caller decides whether to Rename first.
func (s *Store) Add(ssURL, suggestedName string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ssURL = strings.TrimSpace(ssURL)
	if !strings.HasPrefix(ssURL, "ss://") {
		return Key{}, fmt.Errorf("url must start with ss://")
	}
	if len(s.state.Keys) >= maxKeys {
		return Key{}, fmt.Errorf("too many keys (max %d)", maxKeys)
	}
	// Reject duplicate URLs — same server, no point storing twice.
	for _, k := range s.state.Keys {
		if k.URL == ssURL {
			return Key{}, fmt.Errorf("url already stored as key %q", k.Name)
		}
	}

	label := extractLabel(ssURL)
	name := suggestedName
	if name == "" {
		name = deriveName(label, s.state.Keys)
	}
	if err := validateName(name); err != nil {
		return Key{}, err
	}
	for _, k := range s.state.Keys {
		if k.Name == name {
			return Key{}, fmt.Errorf("name %q already in use", name)
		}
	}

	k := Key{
		Name:         name,
		URL:          ssURL,
		LabelFromURL: label,
		AddedAt:      time.Now().UTC(),
	}
	s.state.Keys = append(s.state.Keys, k)
	// If this is the first key, make it active.
	if s.state.Active == "" {
		s.state.Active = name
	}
	if err := s.persist(); err != nil {
		// Roll back in-memory change on persist failure.
		s.state.Keys = s.state.Keys[:len(s.state.Keys)-1]
		if len(s.state.Keys) == 0 {
			s.state.Active = ""
		}
		return Key{}, err
	}
	return k, nil
}

// Remove deletes a key by name. If it was the active key, the active
// pointer is cleared (caller can Use another).
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i, k := range s.state.Keys {
		if k.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no key named %q", name)
	}
	old := s.state.Keys[idx]
	s.state.Keys = append(s.state.Keys[:idx], s.state.Keys[idx+1:]...)
	oldActive := s.state.Active
	if s.state.Active == name {
		s.state.Active = ""
	}
	if err := s.persist(); err != nil {
		// Roll back.
		s.state.Keys = append(s.state.Keys, old)
		s.state.Active = oldActive
		return err
	}
	return nil
}

// Rename changes a key's short name. Active pointer follows.
func (s *Store) Rename(oldName, newName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateName(newName); err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}
	for _, k := range s.state.Keys {
		if k.Name == newName {
			return fmt.Errorf("name %q already in use", newName)
		}
	}
	found := false
	for i := range s.state.Keys {
		if s.state.Keys[i].Name == oldName {
			s.state.Keys[i].Name = newName
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no key named %q", oldName)
	}
	oldActive := s.state.Active
	if s.state.Active == oldName {
		s.state.Active = newName
	}
	if err := s.persist(); err != nil {
		// Roll back rename and active-pointer change.
		for i := range s.state.Keys {
			if s.state.Keys[i].Name == newName {
				s.state.Keys[i].Name = oldName
				break
			}
		}
		s.state.Active = oldActive
		return err
	}
	return nil
}

// Use sets the active key without connecting.
func (s *Store) Use(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, k := range s.state.Keys {
		if k.Name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no key named %q", name)
	}
	old := s.state.Active
	s.state.Active = name
	if err := s.persist(); err != nil {
		s.state.Active = old
		return err
	}
	return nil
}

// MarkConnected records that this key was just used for a successful
// connect. Called by the daemon after `connect` succeeds.
func (s *Store) MarkConnected(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, k := range s.state.Keys {
		if k.Name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no key named %q", name)
	}
	oldActive := s.state.Active
	oldTS := s.state.LastConnectedAt
	s.state.Active = name
	s.state.LastConnectedAt = time.Now().UTC()
	if err := s.persist(); err != nil {
		s.state.Active = oldActive
		s.state.LastConnectedAt = oldTS
		return err
	}
	return nil
}

// --- helpers ---

var nameOK = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("name required")
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("name too long (max %d)", maxNameLen)
	}
	if !nameOK.MatchString(name) {
		return fmt.Errorf("name %q must match [a-z0-9][a-z0-9_-]{0,31}", name)
	}
	return nil
}

// extractLabel returns the URL fragment ("#Home Server") URL-decoded,
// or "" if none.
func extractLabel(ssURL string) string {
	i := strings.LastIndex(ssURL, "#")
	if i < 0 {
		return ""
	}
	raw := ssURL[i+1:]
	if dec, err := url.QueryUnescape(raw); err == nil {
		return dec
	}
	return raw
}

// deriveName picks a short, unique, valid name from the label. Falls
// back to "key-N" if the label can't be normalised.
func deriveName(label string, existing []Key) string {
	base := normalize(label)
	if base == "" {
		base = "key"
	}
	names := map[string]bool{}
	for _, k := range existing {
		names[k.Name] = true
	}
	if !names[base] && nameOK.MatchString(base) {
		return base
	}
	for i := 1; i < 10000; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if len(candidate) > maxNameLen {
			candidate = fmt.Sprintf("key-%d", i)
		}
		if !names[candidate] && nameOK.MatchString(candidate) {
			return candidate
		}
	}
	// Extremely unlikely — but never return an invalid name.
	return "key"
}

// normalize lowercases and replaces non-[a-z0-9-_] with '-', collapsing
// runs and trimming.
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-_")
	if len(out) > maxNameLen {
		out = out[:maxNameLen]
	}
	// Must start with alphanumeric per nameOK.
	for len(out) > 0 {
		c := out[0]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			break
		}
		out = out[1:]
	}
	return out
}
