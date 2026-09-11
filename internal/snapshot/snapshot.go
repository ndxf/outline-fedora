// Package snapshot writes the pre-connect host-state snapshot that
// ouf-panic reads to revert. The format is defined in docs/snapshot-format.md
// and MUST stay wire-compatible with the shell script.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	Version         = 1
	DefaultStateDir = "/var/lib/outline-fedora"
	SnapshotFile    = "snapshot.json"
	BackupSubdir    = "backup"
)

type Snapshot struct {
	Version   int       `json:"version"`
	StartedAt time.Time `json:"started_at"`
	OurMarks  OurMarks  `json:"our_marks"`
	CreatedBy CreatedBy `json:"created_by"`
	PreState  PreState  `json:"pre_state"`
	Actions   []Action  `json:"actions"`
}

type OurMarks struct {
	TunIfname     string  `json:"tun_ifname"`
	TableID       int     `json:"table_id"`
	RulePriority  int     `json:"rule_priority"`
	Fwmark        *uint32 `json:"fwmark"`
}

type CreatedBy struct {
	Hostname   string `json:"hostname"`
	PID        int    `json:"pid"`
	OUFVersion string `json:"ouf_version"`
}

type PreState struct {
	ResolvConf  ResolvConfState `json:"resolv_conf"`
	TunExisted  bool            `json:"tun_existed"`
	Sysctls     []SysctlState   `json:"sysctls"`
}

type ResolvConfState struct {
	Existed       bool   `json:"existed"`
	BackupPath    string `json:"backup_path"`
	WasSymlink    bool   `json:"was_symlink"`
	SymlinkTarget string `json:"symlink_target"`
	SHA256Before  string `json:"sha256_before"`
}

type SysctlState struct {
	Key         string `json:"key"`
	ValueBefore string `json:"value_before"`
}

type Action struct {
	Seq        int    `json:"seq"`
	Op         string `json:"op"`
	Done       bool   `json:"done"`
	// Op-specific fields (omitempty in JSON but not in struct — validated
	// per op type in the daemon).
	Ifname      string `json:"ifname,omitempty"`
	Table       int    `json:"table,omitempty"`
	Spec        string `json:"spec,omitempty"`
	Priority    int    `json:"priority,omitempty"`
	Key         string `json:"key,omitempty"`
	Value       string `json:"value,omitempty"`
	Path        string `json:"path,omitempty"`
	SHA256After string `json:"sha256_after,omitempty"`
}

// Op constants — the closed set the panic script understands.
const (
	OpTunCreate   = "tun_create"
	OpRouteAdd    = "route_add"
	OpRuleAdd     = "rule_add"
	OpSysctlSet   = "sysctl_set"
	OpResolvWrite = "resolv_write"
)

// Writer manages atomic writes of the snapshot file plus backup files.
// It is not goroutine-safe; the daemon should own exactly one Writer per
// active connect attempt.
type Writer struct {
	stateDir  string
	snapshot  *Snapshot
	nextSeq   int
}

// New returns a Writer with an empty snapshot pre-populated with marks
// and metadata. The caller then fills PreState via RecordXxx methods,
// writes the file with Persist, and appends Actions as they occur.
func New(stateDir string, marks OurMarks, oufVersion string) (*Writer, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, BackupSubdir), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir backup dir: %w", err)
	}
	hostname, _ := os.Hostname()
	return &Writer{
		stateDir: stateDir,
		snapshot: &Snapshot{
			Version:   Version,
			StartedAt: time.Now().UTC(),
			OurMarks:  marks,
			CreatedBy: CreatedBy{
				Hostname:   hostname,
				PID:        os.Getpid(),
				OUFVersion: oufVersion,
			},
			PreState: PreState{
				ResolvConf: ResolvConfState{Existed: false},
				Sysctls:    []SysctlState{},
			},
			Actions: []Action{},
		},
		nextSeq: 1,
	}, nil
}

// RecordResolvConf inspects resolvPath and records its state. If it's a
// regular file, its contents are copied to the backup dir and hashed.
// If it's a symlink, only the target is recorded (no content backup —
// the target file is not ours to touch or copy).
func (w *Writer) RecordResolvConf(resolvPath string) error {
	fi, err := os.Lstat(resolvPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			w.snapshot.PreState.ResolvConf = ResolvConfState{Existed: false}
			return nil
		}
		return fmt.Errorf("lstat %s: %w", resolvPath, err)
	}

	if fi.Mode()&os.ModeSymlink != 0 {
		tgt, err := os.Readlink(resolvPath)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", resolvPath, err)
		}
		w.snapshot.PreState.ResolvConf = ResolvConfState{
			Existed:       true,
			WasSymlink:    true,
			SymlinkTarget: tgt,
			// No backup file; on revert we just recreate the symlink.
			BackupPath: "",
		}
		return nil
	}

	// Regular file: copy to backup, hash for tamper detection.
	backupPath := filepath.Join(w.stateDir, BackupSubdir, "resolv.conf")
	if err := copyFile(resolvPath, backupPath, 0o600); err != nil {
		return fmt.Errorf("backup resolv.conf: %w", err)
	}
	sum, err := hashFile(backupPath)
	if err != nil {
		return fmt.Errorf("hash resolv.conf backup: %w", err)
	}
	w.snapshot.PreState.ResolvConf = ResolvConfState{
		Existed:      true,
		BackupPath:   backupPath,
		WasSymlink:   false,
		SHA256Before: sum,
	}
	return nil
}

// RecordSysctl reads the current value of key and records it for later
// restoration. Empty valueBefore means the key was unreadable — we still
// record it so a revert attempt logs a warning rather than silently
// skipping.
func (w *Writer) RecordSysctl(key, valueBefore string) {
	w.snapshot.PreState.Sysctls = append(w.snapshot.PreState.Sysctls,
		SysctlState{Key: key, ValueBefore: valueBefore})
}

// SetTunExisted records whether the tun device already existed before
// we started. If it did, that's a caller error (name collision) — the
// daemon should refuse to connect. Recorded for diagnostics.
func (w *Writer) SetTunExisted(existed bool) {
	w.snapshot.PreState.TunExisted = existed
}

// AppendAction adds an action to the snapshot. Callers typically append
// with Done=false BEFORE the underlying mutation, Persist, then call
// MarkLastDone + Persist after success. This ordering makes revert
// safe even if the process crashes between AppendAction and success.
func (w *Writer) AppendAction(a Action) {
	a.Seq = w.nextSeq
	w.nextSeq++
	w.snapshot.Actions = append(w.snapshot.Actions, a)
}

// MarkLastDone flips the most recently appended action to Done=true.
// No-op if no actions have been appended.
func (w *Writer) MarkLastDone() {
	n := len(w.snapshot.Actions)
	if n == 0 {
		return
	}
	w.snapshot.Actions[n-1].Done = true
}

// Persist writes the snapshot to disk atomically. Safe to call
// repeatedly — each call overwrites the previous file. On any successful
// return, ouf-panic can revert cleanly from what's on disk.
func (w *Writer) Persist() error {
	final := filepath.Join(w.stateDir, SnapshotFile)
	tmp := final + ".tmp"

	data, err := json.MarshalIndent(w.snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename tmp: %w", err)
	}
	return nil
}

// Clear removes the snapshot file. Call after a successful revert — the
// absence of the file is the "clean" signal.
func (w *Writer) Clear() error {
	err := os.Remove(filepath.Join(w.stateDir, SnapshotFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Path returns the on-disk location of the snapshot file.
func (w *Writer) Path() string {
	return filepath.Join(w.stateDir, SnapshotFile)
}

// --- Action constructors ---

func TunCreateAction(ifname string, done bool) Action {
	return Action{Op: OpTunCreate, Ifname: ifname, Done: done}
}

func RouteAddAction(table int, spec string, done bool) Action {
	return Action{Op: OpRouteAdd, Table: table, Spec: spec, Done: done}
}

func RuleAddAction(priority, table int, spec string, done bool) Action {
	return Action{Op: OpRuleAdd, Priority: priority, Table: table, Spec: spec, Done: done}
}

func SysctlSetAction(key, value string, done bool) Action {
	return Action{Op: OpSysctlSet, Key: key, Value: value, Done: done}
}

func ResolvWriteAction(path, sha256After string, done bool) Action {
	return Action{Op: OpResolvWrite, Path: path, SHA256After: sha256After, Done: done}
}

// --- File helpers ---

func copyFile(src, dst string, mode os.FileMode) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(df, sf); err != nil {
		df.Close()
		return err
	}
	if err := df.Sync(); err != nil {
		df.Close()
		return err
	}
	return df.Close()
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
