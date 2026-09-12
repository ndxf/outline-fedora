//go:build linux

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 ndxf

package tun

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"

	"github.com/ndxf/outline-fedora/internal/snapshot"
)

func (s *Session) installRouting() error {
	// Route: 10.233.233.2/32 dev outline-tun0 scope link src 10.233.233.1
	gwNet, err := netlink.ParseIPNet(s.cfg.TunGatewayCIDR)
	if err != nil {
		return fmt.Errorf("parse gateway cidr %q: %w", s.cfg.TunGatewayCIDR, err)
	}
	r1 := netlink.Route{
		LinkIndex: s.tun.link.Attrs().Index,
		Table:     s.cfg.RoutingTableID,
		Dst:       gwNet,
		Src:       net.ParseIP(s.cfg.TunIP),
		Scope:     netlink.SCOPE_LINK,
	}
	specGW := fmt.Sprintf("%s dev %s scope link src %s", gwNet, s.cfg.TunName, s.cfg.TunIP)
	// Snapshot BEFORE the mutation. If we crash between AppendAction/Persist
	// and RouteAdd, the panic script does route flush on table N — a no-op
	// on an empty table. Safe.
	s.snap.AppendAction(snapshot.RouteAddAction(s.cfg.RoutingTableID, specGW, false))
	if err := s.snap.Persist(); err != nil {
		return err
	}
	if err := netlink.RouteAdd(&r1); err != nil {
		return fmt.Errorf("add gw route (table %d): %w", s.cfg.RoutingTableID, err)
	}
	s.markLastActionDone()
	if err := s.snap.Persist(); err != nil {
		return err
	}

	// Route: default via 10.233.233.2 dev outline-tun0
	r2 := netlink.Route{
		LinkIndex: s.tun.link.Attrs().Index,
		Table:     s.cfg.RoutingTableID,
		Gw:        gwNet.IP,
	}
	specDef := fmt.Sprintf("default via %s dev %s", gwNet.IP, s.cfg.TunName)
	s.snap.AppendAction(snapshot.RouteAddAction(s.cfg.RoutingTableID, specDef, false))
	if err := s.snap.Persist(); err != nil {
		return err
	}
	if err := netlink.RouteAdd(&r2); err != nil {
		return fmt.Errorf("add default route (table %d): %w", s.cfg.RoutingTableID, err)
	}
	s.markLastActionDone()
	if err := s.snap.Persist(); err != nil {
		return err
	}

	// Rules (installed in reverse priority order so the ss-server bypass
	// runs first — lower priority = evaluated first).
	//
	// Goal: internet traffic goes via the tun, but the following are
	// preserved via the main table:
	//   - the ss:// server itself (else infinite tunnel loop)
	//   - RFC1918 private networks (so LAN, SSH-in, libvirt bridges keep working)
	//   - loopback (127/8)
	//   - link-local (169.254/16)
	//   - multicast (224/4)
	//
	// One rule per exclusion, all at priorities near cfg.RoutingRulePriority
	// (documented in the snapshot so ouf-panic removes each one).
	svrCIDR := s.svrIP.String() + "/32"

	bypasses := []struct {
		cidr string
		desc string
	}{
		{svrCIDR, "shadowsocks server"},
		{"10.0.0.0/8", "private 10/8"},
		{"172.16.0.0/12", "private 172.16/12"},
		{"192.168.0.0/16", "private 192.168/16"},
		{"127.0.0.0/8", "loopback"},
		{"169.254.0.0/16", "link-local"},
		{"224.0.0.0/4", "multicast"},
	}
	// Bypass rules: `to <cidr> lookup main` at lower priorities so they
	// win over the catchall.
	for i, b := range bypasses {
		pri := s.cfg.RoutingRulePriority - len(bypasses) + i
		if pri < 1 {
			return fmt.Errorf("bypass rule priority underflow (base %d)", s.cfg.RoutingRulePriority)
		}
		dst, err := netlink.ParseIPNet(b.cidr)
		if err != nil {
			return fmt.Errorf("parse bypass cidr %q: %w", b.cidr, err)
		}
		rule := netlink.NewRule()
		rule.Priority = pri
		rule.Family = netlink.FAMILY_V4
		rule.Table = 254 // main
		rule.Dst = dst
		spec := fmt.Sprintf("to %s lookup main (bypass %s)", b.cidr, b.desc)
		s.snap.AppendAction(snapshot.RuleAddAction(pri, 254, spec, false))
		if err := s.snap.Persist(); err != nil {
			return err
		}
		if err := netlink.RuleAdd(rule); err != nil {
			return fmt.Errorf("add bypass rule pri=%d dst=%s: %w", pri, b.cidr, err)
		}
		s.markLastActionDone()
		if err := s.snap.Persist(); err != nil {
			return err
		}
	}

	// Catchall: everything else goes to our table (the default via ouftun0).
	rule := netlink.NewRule()
	rule.Priority = s.cfg.RoutingRulePriority
	rule.Family = netlink.FAMILY_V4
	rule.Table = s.cfg.RoutingTableID

	specRule := fmt.Sprintf("from all lookup %d (catchall)", s.cfg.RoutingTableID)
	s.snap.AppendAction(snapshot.RuleAddAction(s.cfg.RoutingRulePriority, s.cfg.RoutingTableID, specRule, false))
	if err := s.snap.Persist(); err != nil {
		return err
	}
	if err := netlink.RuleAdd(rule); err != nil {
		return fmt.Errorf("add catchall rule pri=%d: %w", s.cfg.RoutingRulePriority, err)
	}
	s.markLastActionDone()
	if err := s.snap.Persist(); err != nil {
		return err
	}
	return nil
}

// markLastActionDone flips the trailing action's Done to true. Called
// right after the underlying netlink/write actually succeeds. This lets
// us tell (from the snapshot alone) which actions completed vs. which
// were merely intended.
func (s *Session) markLastActionDone() {
	// Access via the writer's Snapshot field is intentional — we own
	// the writer for this session, no cross-goroutine access.
	s.snap.MarkLastDone()
}
