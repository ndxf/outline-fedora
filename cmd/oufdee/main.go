// oufdee: the outline-fedora daemon. Runs as root under systemd, owns
// the TUN + routing + DNS mutations. CLI talks to it over a Unix
// socket at /run/outline-fedora.sock.
package main

import (
	"context"
	"flag"
	"log"
	"os"
)

var version = "dev"

func main() {
	socket := flag.String("socket", "/run/outline-fedora.sock", "unix socket path")
	group := flag.String("group", "wheel", "group to chown the socket to (empty=skip)")
	keys := flag.String("keys", "/etc/outline-fedora/keys.json", "keystore path")
	stateDir := flag.String("state-dir", "/var/lib/outline-fedora", "state/snapshot dir")
	resolvPath := flag.String("resolv", "/etc/resolv.conf", "resolv.conf path to manage")
	flag.Parse()

	logger := log.New(os.Stderr, "oufdee: ", log.LstdFlags|log.Lmicroseconds)

	srv, err := NewServer(ServerConfig{
		SocketPath:  *socket,
		SocketGroup: *group,
		KeysPath:    *keys,
		StateDir:    *stateDir,
		ResolvPath:  *resolvPath,
		OUFVersion:  version,
	}, logger)
	if err != nil {
		logger.Fatalf("init: %v", err)
	}

	if err := srv.Run(context.Background()); err != nil {
		logger.Fatalf("run: %v", err)
	}
}
