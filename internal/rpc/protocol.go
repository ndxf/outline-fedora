// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 ndxf

// Package rpc defines the wire format between the ouf CLI (unprivileged
// client) and oufdee (privileged daemon), spoken over a Unix domain
// socket at /run/outline-fedora.sock.
//
// Framing: one JSON object per newline. Client sends a Request, server
// sends exactly one Response, connection closes. No pipelining, no
// streaming — a fresh connection per RPC keeps the state machine
// trivial and matches the CLI's per-command lifecycle.
package rpc

import "time"

const (
	// SocketPath is where oufdee listens. Group-owned by wheel so
	// members of wheel can talk to it without sudo. Lives inside the
	// systemd RuntimeDirectory so it appears/disappears with the unit
	// and sandboxing (ReadWritePaths etc.) works cleanly.
	SocketPath = "/run/outline-fedora/oufdee.sock"
)

// Method names — closed set. Any unknown method returns an error.
const (
	MethodList          = "list"
	MethodAdd           = "add"
	MethodRemove        = "remove"
	MethodRename        = "rename"
	MethodUse           = "use"
	MethodConnect       = "connect"
	MethodDisconnect    = "disconnect"
	MethodStatus        = "status"
	MethodUndo          = "undo"
	MethodPing          = "ping"
)

// Request is what the CLI sends. Only fields relevant to Method are
// populated; the rest are ignored on the server.
type Request struct {
	Method string `json:"method"`

	// For add
	URL          string `json:"url,omitempty"`
	SuggestedName string `json:"suggested_name,omitempty"`

	// For remove, rename (old), use, connect
	Name string `json:"name,omitempty"`

	// For rename (new)
	NewName string `json:"new_name,omitempty"`
}

// Response — one of these per Request. Ok=false means Err is set.
type Response struct {
	Ok  bool   `json:"ok"`
	Err string `json:"err,omitempty"`

	// Method-specific payloads. Only the field matching the request
	// method is populated on success.
	Keys       []KeyView   `json:"keys,omitempty"`         // list
	Active     string      `json:"active,omitempty"`       // list
	Added      *KeyView    `json:"added,omitempty"`        // add
	Connected  *StatusView `json:"connected,omitempty"`    // connect, status
	Status     *StatusView `json:"status,omitempty"`       // status
}

// KeyView is the client-facing shape of a stored key. LabelFromURL is
// cosmetic, Name is what commands take.
type KeyView struct {
	Name         string    `json:"name"`
	URL          string    `json:"url"`
	LabelFromURL string    `json:"label_from_url,omitempty"`
	AddedAt      time.Time `json:"added_at"`
	Active       bool      `json:"active"`
	Connected    bool      `json:"connected"`
}

// StatusView describes the daemon's current connection state.
type StatusView struct {
	Connected      bool      `json:"connected"`
	KeyName        string    `json:"key_name,omitempty"`
	Since          time.Time `json:"since,omitempty"`
	ServerIP       string    `json:"server_ip,omitempty"`
	TunName        string    `json:"tun_name,omitempty"`
	DirtySnapshot  bool      `json:"dirty_snapshot"` // snapshot.json present when not connected -> revert pending
}
