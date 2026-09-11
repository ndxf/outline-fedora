//go:build !linux

package tun

import (
	"context"
	"errors"
	"net"
)

type Session struct{ cfg Config }

func New(cfg Config) (*Session, error) {
	return nil, errors.New("outline-fedora is Linux-only")
}

func (s *Session) Start(ctx context.Context) error { return errors.New("linux-only") }
func (s *Session) Wait()                            {}
func (s *Session) Close() error                     { return nil }
func (s *Session) ServerIP() net.IP                 { return nil }
