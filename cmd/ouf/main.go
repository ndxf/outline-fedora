// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 ndxf

// ouf: the outline-fedora CLI. Unprivileged; sends RPCs to oufdee over
// /run/outline-fedora.sock. The `undo` subcommand also has a fallback
// path that invokes ouf-panic directly if the daemon is unreachable.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ndxf/outline-fedora/internal/rpc"
)

// version is populated at build time via -ldflags "-X main.version=..."
var version = "dev"

var (
	socketPath = flag.String("socket", rpc.SocketPath, "path to oufdee socket")
	panicPath  = flag.String("panic-script", "/usr/local/sbin/ouf-panic", "path to ouf-panic (used by undo fallback)")
	showVersion = flag.Bool("version", false, "print version and exit")
)

func main() {
	flag.Usage = usage
	flag.Parse()
	if *showVersion {
		fmt.Printf("ouf %s\n", version)
		return
	}
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]

	var err error
	switch cmd {
	case "help", "-h", "--help":
		usage()
	case "add":
		err = doAdd(rest)
	case "list", "ls":
		err = doList(rest)
	case "remove", "rm":
		err = doRemove(rest)
	case "rename", "mv":
		err = doRename(rest)
	case "use":
		err = doUse(rest)
	case "connect", "switch":
		err = doConnect(rest)
	case "disconnect", "down":
		err = doDisconnect(rest)
	case "status":
		err = doStatus(rest)
	case "undo":
		err = doUndo(rest)
	case "ping":
		err = doPing(rest)
	default:
		fmt.Fprintf(os.Stderr, "ouf: unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ouf: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ouf: outline-fedora client

USAGE
  ouf add <ss://...> [--name NAME]
  ouf list
  ouf remove <name>
  ouf rename <old> <new>
  ouf use <name>                    change default (no connect)
  ouf connect [name]                connect (uses active if no arg)
  ouf switch <name>                 alias for connect
  ouf disconnect                    disconnect and revert host state
  ouf status
  ouf undo                          revert host state (works even if daemon dead)
  ouf ping                          test daemon liveness

GLOBAL FLAGS
`)
	flag.PrintDefaults()
}

// --- transport ---

func send(req *rpc.Request) (*rpc.Response, error) {
	c, err := net.DialTimeout("unix", *socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon at %s: %w (is oufdee running?)", *socketPath, err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, err
	}
	if err := rpc.WriteMessage(c, req); err != nil {
		return nil, err
	}
	var resp rpc.Response
	if err := rpc.ReadMessage(bufio.NewReader(c), &resp); err != nil {
		return nil, err
	}
	if !resp.Ok {
		return nil, errors.New(resp.Err)
	}
	return &resp, nil
}

// --- subcommands ---

func doAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	name := fs.String("name", "", "short name (auto-derived if empty)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: ouf add <ss://...> [--name NAME]")
	}
	resp, err := send(&rpc.Request{
		Method:        rpc.MethodAdd,
		URL:           fs.Arg(0),
		SuggestedName: *name,
	})
	if err != nil {
		return err
	}
	if resp.Added != nil {
		fmt.Printf("added %s\n", resp.Added.Name)
	}
	return nil
}

func doList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "output as JSON")
	fs.Parse(args)
	resp, err := send(&rpc.Request{Method: rpc.MethodList})
	if err != nil {
		return err
	}
	if *jsonOut {
		json.NewEncoder(os.Stdout).Encode(resp)
		return nil
	}
	if len(resp.Keys) == 0 {
		fmt.Println("(no keys — add one with 'ouf add ss://...')")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tACTIVE\tCONNECTED\tLABEL\tHOST")
	for _, k := range resp.Keys {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			k.Name,
			flag2(k.Active), flag2(k.Connected),
			truncate(k.LabelFromURL, 24),
			hostOfSS(k.URL),
		)
	}
	tw.Flush()
	return nil
}

func doRemove(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ouf remove <name>")
	}
	_, err := send(&rpc.Request{Method: rpc.MethodRemove, Name: args[0]})
	if err == nil {
		fmt.Printf("removed %s\n", args[0])
	}
	return err
}

func doRename(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: ouf rename <old> <new>")
	}
	_, err := send(&rpc.Request{Method: rpc.MethodRename, Name: args[0], NewName: args[1]})
	if err == nil {
		fmt.Printf("renamed %s -> %s\n", args[0], args[1])
	}
	return err
}

func doUse(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ouf use <name>")
	}
	_, err := send(&rpc.Request{Method: rpc.MethodUse, Name: args[0]})
	if err == nil {
		fmt.Printf("active key set to %s (not connecting)\n", args[0])
	}
	return err
}

func doConnect(args []string) error {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	resp, err := send(&rpc.Request{Method: rpc.MethodConnect, Name: name})
	if err != nil {
		return err
	}
	if resp.Connected != nil {
		fmt.Printf("connected: key=%s server=%s\n", resp.Connected.KeyName, resp.Connected.ServerIP)
	} else {
		fmt.Println("connected")
	}
	return nil
}

func doDisconnect(args []string) error {
	_, err := send(&rpc.Request{Method: rpc.MethodDisconnect})
	if err == nil {
		fmt.Println("disconnected; host state reverted")
	}
	return err
}

func doStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "output as JSON")
	fs.Parse(args)
	resp, err := send(&rpc.Request{Method: rpc.MethodStatus})
	if err != nil {
		return err
	}
	if *jsonOut {
		json.NewEncoder(os.Stdout).Encode(resp.Status)
		return nil
	}
	sv := resp.Status
	if sv == nil || !sv.Connected {
		fmt.Println("not connected")
		if sv != nil && sv.DirtySnapshot {
			fmt.Println("WARNING: a dirty snapshot exists — run 'ouf undo' to revert")
		}
		return nil
	}
	fmt.Printf("connected\n")
	fmt.Printf("  key:    %s\n", sv.KeyName)
	fmt.Printf("  server: %s\n", sv.ServerIP)
	fmt.Printf("  since:  %s (%s ago)\n", sv.Since.Format(time.RFC3339), time.Since(sv.Since).Round(time.Second))
	return nil
}

// doUndo tries the daemon first; if it can't reach the daemon, invokes
// ouf-panic directly. This is the promise: undo works even if the
// daemon is dead.
func doUndo(args []string) error {
	fs := flag.NewFlagSet("undo", flag.ExitOnError)
	stateDir := fs.String("state-dir", "/var/lib/outline-fedora", "state dir (fallback path only)")
	resolvPath := fs.String("resolv", "/etc/resolv.conf", "resolv.conf path (fallback path only)")
	fs.Parse(args)

	// Try daemon.
	if _, err := send(&rpc.Request{Method: rpc.MethodUndo}); err == nil {
		fmt.Println("undo done via daemon")
		return nil
	} else {
		fmt.Fprintf(os.Stderr, "daemon unreachable (%v); falling back to direct panic script\n", err)
	}

	// Fallback: exec the panic script directly. Requires root (writing
	// /etc/resolv.conf, ip commands). If not root, exec via sudo.
	if os.Geteuid() != 0 {
		return runViaSudo(*panicPath, *stateDir, *resolvPath)
	}
	return runPanic(*panicPath, *stateDir, *resolvPath)
}

func runPanic(script, stateDir, resolvPath string) error {
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("panic script not found at %s (install it or pass --panic-script)", script)
	}
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(),
		"OUF_STATE_DIR="+stateDir,
		"OUF_RESOLV_PATH="+resolvPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runViaSudo(script, stateDir, resolvPath string) error {
	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("need root to run panic script; sudo not found either")
	}
	cmd := exec.Command("sudo",
		"OUF_STATE_DIR="+stateDir,
		"OUF_RESOLV_PATH="+resolvPath,
		script,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func doPing(args []string) error {
	_, err := send(&rpc.Request{Method: rpc.MethodPing})
	if err == nil {
		fmt.Println("pong")
	}
	return err
}

// --- format helpers ---

func flag2(b bool) string {
	if b {
		return "yes"
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// hostOfSS pulls host:port from an ss:// URL without exposing the credential.
// Returns "?" on parse failure.
func hostOfSS(u string) string {
	const p = "ss://"
	if !strings.HasPrefix(u, p) {
		return "?"
	}
	rest := u[len(p):]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return "?"
	}
	rest = rest[at+1:]
	if hash := strings.Index(rest, "#"); hash >= 0 {
		rest = rest[:hash]
	}
	return rest
}

// Ensure imports we may lose if the file gets reformatted.
var (
	_ = filepath.Join
	_ = io.EOF
)
