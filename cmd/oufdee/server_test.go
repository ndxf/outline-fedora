package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ndxf/outline-fedora/internal/rpc"
	"github.com/ndxf/outline-fedora/internal/tun"
)

// TestServerEndToEnd runs the whole daemon in-process and drives it
// with real RPC calls. TUN mutations still need CAP_NET_ADMIN so the
// full connect path is only exercised when OUF_TEST_IN_NETNS=1; without
// that, only the non-mutating RPCs are tested (add/list/remove/etc).
func TestServerEndToEnd(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")
	keysPath := filepath.Join(dir, "keys.json")
	stateDir := filepath.Join(dir, "state")
	resolvPath := filepath.Join(dir, "fake-resolv.conf")
	if err := os.WriteFile(resolvPath, []byte("nameserver 192.168.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wire the tun package's panic-script env var so Session.Close
	// finds the script.
	panicScript := findPanicScript(t)
	tun.PanicScriptPath = panicScript
	t.Setenv("OUF_PANIC_SCRIPT", panicScript)

	srv, err := NewServer(ServerConfig{
		SocketPath:  sockPath,
		SocketGroup: "", // skip chgrp in tests
		KeysPath:    keysPath,
		StateDir:    stateDir,
		ResolvPath:  resolvPath,
		OUFVersion:  "test",
	}, log.New(os.Stderr, "test-oufdee: ", log.LstdFlags))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Wait for the socket to appear (up to 2s).
	waitSocket(t, sockPath)

	// --- add ---
	ssURL := fakeSSURL("1.1.1.1", "e2e")
	resp := call(t, sockPath, &rpc.Request{
		Method: rpc.MethodAdd, URL: ssURL, SuggestedName: "e2e",
	})
	if !resp.Ok {
		t.Fatalf("add: %s", resp.Err)
	}
	if resp.Added == nil || resp.Added.Name != "e2e" {
		t.Fatalf("add returned unexpected: %+v", resp)
	}

	// --- list ---
	resp = call(t, sockPath, &rpc.Request{Method: rpc.MethodList})
	if !resp.Ok || len(resp.Keys) != 1 || resp.Keys[0].Name != "e2e" {
		t.Fatalf("list: %+v", resp)
	}
	if !resp.Keys[0].Active {
		t.Fatal("first added key should be active")
	}
	if resp.Keys[0].Connected {
		t.Fatal("shouldn't be connected yet")
	}

	// --- status while disconnected ---
	resp = call(t, sockPath, &rpc.Request{Method: rpc.MethodStatus})
	if !resp.Ok || resp.Status == nil || resp.Status.Connected {
		t.Fatalf("status expected not-connected: %+v", resp)
	}

	// --- connect (only if we're in a netns) ---
	if os.Getenv("OUF_TEST_IN_NETNS") == "1" {
		resp = call(t, sockPath, &rpc.Request{Method: rpc.MethodConnect, Name: "e2e"})
		if !resp.Ok {
			t.Fatalf("connect: %s", resp.Err)
		}
		if resp.Connected == nil || resp.Connected.KeyName != "e2e" {
			t.Fatalf("connect returned: %+v", resp)
		}

		// status should now say connected
		resp = call(t, sockPath, &rpc.Request{Method: rpc.MethodStatus})
		if !resp.Ok || resp.Status == nil || !resp.Status.Connected {
			t.Fatalf("status after connect: %+v", resp)
		}

		// disconnect
		resp = call(t, sockPath, &rpc.Request{Method: rpc.MethodDisconnect})
		if !resp.Ok {
			t.Fatalf("disconnect: %s", resp.Err)
		}

		// snapshot should be cleared
		if _, err := os.Stat(filepath.Join(stateDir, "snapshot.json")); !os.IsNotExist(err) {
			t.Errorf("snapshot not cleared: %v", err)
		}
	} else {
		t.Log("skipping connect/disconnect path (not in netns)")
	}

	// --- remove ---
	resp = call(t, sockPath, &rpc.Request{Method: rpc.MethodRemove, Name: "e2e"})
	if !resp.Ok {
		t.Fatalf("remove: %s", resp.Err)
	}

	// --- undo when no snapshot ---
	resp = call(t, sockPath, &rpc.Request{Method: rpc.MethodUndo})
	if !resp.Ok {
		t.Fatalf("undo: %s", resp.Err)
	}

	// Shut down the server.
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("server run: %v", err)
	}
}

func call(t *testing.T, sock string, req *rpc.Request) *rpc.Response {
	t.Helper()
	c, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer c.Close()
	if err := rpc.WriteMessage(c, req); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp rpc.Response
	if err := rpc.ReadMessage(bufio.NewReader(c), &resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	return &resp
}

func waitSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("socket never appeared at %s", path)
}

func fakeSSURL(host, tag string) string {
	ui := base64.URLEncoding.EncodeToString([]byte("aes-256-gcm:testpassword"))
	return fmt.Sprintf("ss://%s@%s:8388#%s", ui, host, tag)
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
