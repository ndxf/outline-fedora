package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ndxf/outline-fedora/internal/keystore"
	"github.com/ndxf/outline-fedora/internal/rpc"
	"github.com/ndxf/outline-fedora/internal/tun"
)

// Server owns the daemon's global state: the keystore, the socket
// listener, and the (at most one) active tun.Session.
type Server struct {
	cfg    ServerConfig
	ks     *keystore.Store
	logger *log.Logger

	mu       sync.Mutex
	session  *tun.Session
	sessKey  string    // name of the key powering session, if any
	sessSince time.Time
}

type ServerConfig struct {
	SocketPath    string
	SocketGroup   string // if non-empty, chgrp the socket to this
	KeysPath      string
	StateDir      string
	ResolvPath    string
	OUFVersion    string
}

func (c *ServerConfig) applyDefaults() {
	if c.SocketPath == "" {
		c.SocketPath = rpc.SocketPath
	}
	if c.KeysPath == "" {
		c.KeysPath = keystore.DefaultPath
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/outline-fedora"
	}
	if c.ResolvPath == "" {
		c.ResolvPath = "/etc/resolv.conf"
	}
	if c.OUFVersion == "" {
		c.OUFVersion = "dev"
	}
}

// NewServer opens the keystore and prepares state, but does NOT bind
// the socket yet. Call Run to bind and accept.
func NewServer(cfg ServerConfig, logger *log.Logger) (*Server, error) {
	cfg.applyDefaults()
	if logger == nil {
		logger = log.New(os.Stderr, "oufdee: ", log.LstdFlags)
	}
	ks, err := keystore.Open(cfg.KeysPath)
	if err != nil {
		return nil, fmt.Errorf("open keystore: %w", err)
	}
	return &Server{cfg: cfg, ks: ks, logger: logger}, nil
}

// Run listens on the socket until ctx is done or the process gets
// SIGTERM/SIGINT. On shutdown, it disconnects any active session.
func (s *Server) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.cfg.SocketPath), 0o755); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	// Best-effort remove any stale socket left behind by an unclean
	// prior exit.
	_ = os.Remove(s.cfg.SocketPath)

	l, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.SocketPath, err)
	}
	defer l.Close()

	// Perms: 0660 + group=wheel so wheel members can talk without sudo
	// (matches user's global CLAUDE.md note about being in wheel).
	if err := os.Chmod(s.cfg.SocketPath, 0o660); err != nil {
		s.logger.Printf("warn: chmod socket: %v", err)
	}
	if s.cfg.SocketGroup != "" {
		if err := chgrpSocket(s.cfg.SocketPath, s.cfg.SocketGroup); err != nil {
			s.logger.Printf("warn: chgrp socket to %s: %v", s.cfg.SocketGroup, err)
		}
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case sig := <-sigc:
			s.logger.Printf("received %v, shutting down", sig)
		case <-ctx.Done():
		}
		l.Close()
	}()

	s.logger.Printf("listening on %s", s.cfg.SocketPath)

	var wg sync.WaitGroup
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				break
			}
			s.logger.Printf("accept: %v", err)
			continue
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			defer c.Close()
			s.handle(c)
		}(conn)
	}
	wg.Wait()

	// Shutdown: tear down any active session. Uses the same panic-script
	// revert path as normal disconnect, so nothing is left dirty.
	s.mu.Lock()
	sess := s.session
	s.session = nil
	s.sessKey = ""
	s.mu.Unlock()
	if sess != nil {
		s.logger.Printf("disconnecting active session on shutdown")
		if err := sess.Close(); err != nil {
			s.logger.Printf("shutdown revert failed: %v", err)
			return err
		}
	}
	return nil
}

func (s *Server) handle(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	br := bufio.NewReader(c)
	var req rpc.Request
	if err := rpc.ReadMessage(br, &req); err != nil {
		s.logger.Printf("read req: %v", err)
		return
	}
	resp := s.dispatch(&req)
	if err := rpc.WriteMessage(c, resp); err != nil {
		s.logger.Printf("write resp: %v", err)
	}
}

func (s *Server) dispatch(req *rpc.Request) *rpc.Response {
	switch req.Method {
	case rpc.MethodPing:
		return &rpc.Response{Ok: true}
	case rpc.MethodList:
		return s.doList()
	case rpc.MethodAdd:
		return s.doAdd(req)
	case rpc.MethodRemove:
		return s.doRemove(req)
	case rpc.MethodRename:
		return s.doRename(req)
	case rpc.MethodUse:
		return s.doUse(req)
	case rpc.MethodConnect:
		return s.doConnect(req)
	case rpc.MethodDisconnect:
		return s.doDisconnect()
	case rpc.MethodStatus:
		return s.doStatus()
	case rpc.MethodUndo:
		return s.doUndo()
	default:
		return errResp("unknown method %q", req.Method)
	}
}

// --- handlers ---

func (s *Server) doList() *rpc.Response {
	keys := s.ks.List()
	active := s.ks.Active()
	s.mu.Lock()
	connectedName := s.sessKey
	s.mu.Unlock()
	out := make([]rpc.KeyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, rpc.KeyView{
			Name:         k.Name,
			URL:          k.URL,
			LabelFromURL: k.LabelFromURL,
			AddedAt:      k.AddedAt,
			Active:       k.Name == active,
			Connected:    k.Name == connectedName,
		})
	}
	return &rpc.Response{Ok: true, Keys: out, Active: active}
}

func (s *Server) doAdd(req *rpc.Request) *rpc.Response {
	k, err := s.ks.Add(req.URL, req.SuggestedName)
	if err != nil {
		return errResp("%v", err)
	}
	kv := rpc.KeyView{
		Name: k.Name, URL: k.URL, LabelFromURL: k.LabelFromURL,
		AddedAt: k.AddedAt, Active: s.ks.Active() == k.Name,
	}
	return &rpc.Response{Ok: true, Added: &kv}
}

func (s *Server) doRemove(req *rpc.Request) *rpc.Response {
	// Refuse to remove the currently-connected key; user must
	// disconnect first.
	s.mu.Lock()
	sk := s.sessKey
	s.mu.Unlock()
	if sk == req.Name {
		return errResp("cannot remove %q: currently connected — disconnect first", req.Name)
	}
	if err := s.ks.Remove(req.Name); err != nil {
		return errResp("%v", err)
	}
	return &rpc.Response{Ok: true}
}

func (s *Server) doRename(req *rpc.Request) *rpc.Response {
	if err := s.ks.Rename(req.Name, req.NewName); err != nil {
		return errResp("%v", err)
	}
	// Keep session's key-name pointer in sync if it renamed the active one.
	s.mu.Lock()
	if s.sessKey == req.Name {
		s.sessKey = req.NewName
	}
	s.mu.Unlock()
	return &rpc.Response{Ok: true}
}

func (s *Server) doUse(req *rpc.Request) *rpc.Response {
	if err := s.ks.Use(req.Name); err != nil {
		return errResp("%v", err)
	}
	return &rpc.Response{Ok: true}
}

func (s *Server) doConnect(req *rpc.Request) *rpc.Response {
	name := req.Name
	if name == "" {
		name = s.ks.Active()
	}
	if name == "" {
		return errResp("no key specified and no active key set")
	}
	key, ok := s.ks.Get(name)
	if !ok {
		return errResp("no key named %q", name)
	}

	s.mu.Lock()
	if s.session != nil {
		// If already connected to the SAME key, no-op.
		if s.sessKey == name {
			view := s.statusViewLocked()
			s.mu.Unlock()
			return &rpc.Response{Ok: true, Connected: view}
		}
		// Otherwise, switch: disconnect old, connect new. Do this under
		// the lock briefly to prevent races, but release for the actual
		// Close (which can block on panic script).
		old := s.session
		s.session = nil
		s.sessKey = ""
		s.mu.Unlock()
		if err := old.Close(); err != nil {
			return errResp("switch: could not disconnect previous session: %v", err)
		}
	} else {
		s.mu.Unlock()
	}

	// Fresh connect.
	sess, err := tun.New(tun.Config{
		TransportURL: key.URL,
		StateDir:     s.cfg.StateDir,
		ResolvPath:   s.cfg.ResolvPath,
		OUFVersion:   s.cfg.OUFVersion,
	})
	if err != nil {
		return errResp("build session: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sess.Start(ctx); err != nil {
		return errResp("start session: %v", err)
	}

	s.mu.Lock()
	s.session = sess
	s.sessKey = name
	s.sessSince = time.Now()
	view := s.statusViewLocked()
	s.mu.Unlock()

	if err := s.ks.MarkConnected(name); err != nil {
		s.logger.Printf("warn: MarkConnected(%q): %v", name, err)
	}
	return &rpc.Response{Ok: true, Connected: view}
}

func (s *Server) doDisconnect() *rpc.Response {
	s.mu.Lock()
	sess := s.session
	s.session = nil
	s.sessKey = ""
	s.mu.Unlock()
	if sess == nil {
		return errResp("not connected")
	}
	if err := sess.Close(); err != nil {
		return errResp("close session: %v", err)
	}
	return &rpc.Response{Ok: true}
}

func (s *Server) doStatus() *rpc.Response {
	s.mu.Lock()
	view := s.statusViewLocked()
	s.mu.Unlock()
	return &rpc.Response{Ok: true, Status: view}
}

func (s *Server) doUndo() *rpc.Response {
	// If we have an active session, disconnect it — that runs the
	// panic script and also clears the snapshot. If no session,
	// invoke the panic script directly (it's a no-op if snapshot is
	// absent, or does the revert if a prior crash left one behind).
	s.mu.Lock()
	sess := s.session
	s.session = nil
	s.sessKey = ""
	s.mu.Unlock()
	if sess != nil {
		if err := sess.Close(); err != nil {
			return errResp("session close: %v", err)
		}
		return &rpc.Response{Ok: true}
	}
	// No session — call the panic script standalone.
	if err := tun.RunPanicStandalone(s.cfg.StateDir, s.cfg.ResolvPath); err != nil {
		return errResp("panic script: %v", err)
	}
	return &rpc.Response{Ok: true}
}

func (s *Server) statusViewLocked() *rpc.StatusView {
	// Caller holds s.mu.
	sv := &rpc.StatusView{}
	if s.session != nil {
		sv.Connected = true
		sv.KeyName = s.sessKey
		sv.Since = s.sessSince
		if ip := s.session.ServerIP(); ip != nil {
			sv.ServerIP = ip.String()
		}
	}
	// A snapshot file present while we're not connected means a prior
	// dirty exit — surface this so the CLI can flag it.
	snapPath := filepath.Join(s.cfg.StateDir, "snapshot.json")
	if _, err := os.Stat(snapPath); err == nil && !sv.Connected {
		sv.DirtySnapshot = true
	}
	return sv
}

func errResp(f string, a ...any) *rpc.Response {
	return &rpc.Response{Ok: false, Err: fmt.Sprintf(f, a...)}
}
