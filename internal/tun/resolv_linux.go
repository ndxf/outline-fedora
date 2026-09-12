//go:build linux

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 ndxf

package tun

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/ndxf/outline-fedora/internal/snapshot"
)

// writeResolvConf replaces the resolv.conf at cfg.ResolvPath with a
// single "nameserver <DNS>" entry. If the path was a symlink (typical on
// Fedora, where it points at systemd-resolved's stub), we unlink first
// and write a regular file. Revert (via ouf-panic) recreates the
// symlink from the snapshot.
func (s *Session) writeResolvConf() error {
	// Snapshot already recorded the pre-state in Start().
	content := []byte(fmt.Sprintf(
		"# outline-fedora: managed while VPN is active\nnameserver %s\n",
		s.cfg.DNSServerIP))
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	// If the path is currently a symlink, unlink it. Otherwise
	// overwrite in place with atomic rename.
	if fi, err := os.Lstat(s.cfg.ResolvPath); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(s.cfg.ResolvPath); err != nil {
				return fmt.Errorf("unlink symlink %s: %w", s.cfg.ResolvPath, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("lstat %s: %w", s.cfg.ResolvPath, err)
	}

	// Snapshot BEFORE the write. Even if the write is the very last
	// thing we do, snapshotting first makes the revert-on-crash story
	// consistent (panic script sees the intent and can flush the file
	// back — file is already backed up).
	s.snap.AppendAction(snapshot.ResolvWriteAction(s.cfg.ResolvPath, sha, false))
	if err := s.snap.Persist(); err != nil {
		return err
	}

	tmp := s.cfg.ResolvPath + ".ouf.tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return fmt.Errorf("write tmp resolv: %w", err)
	}
	if err := os.Rename(tmp, s.cfg.ResolvPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename tmp -> resolv: %w", err)
	}
	s.snap.MarkLastDone()
	if err := s.snap.Persist(); err != nil {
		return err
	}
	return nil
}
