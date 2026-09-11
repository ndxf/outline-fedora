//go:build linux

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

	// Rule: from all not to <server>/32 lookup <table>. Traffic to the
	// SS server itself must NOT go through the tun (that would be an
	// infinite loop through the tunnel); it goes via the main table.
	svrCIDR := s.svrIP.String() + "/32"
	svrNet, err := netlink.ParseIPNet(svrCIDR)
	if err != nil {
		return fmt.Errorf("parse server cidr %q: %w", svrCIDR, err)
	}
	rule := netlink.NewRule()
	rule.Priority = s.cfg.RoutingRulePriority
	rule.Family = netlink.FAMILY_V4
	rule.Table = s.cfg.RoutingTableID
	rule.Dst = svrNet
	rule.Invert = true

	specRule := fmt.Sprintf("from all not to %s lookup %d", svrCIDR, s.cfg.RoutingTableID)
	s.snap.AppendAction(snapshot.RuleAddAction(s.cfg.RoutingRulePriority, s.cfg.RoutingTableID, specRule, false))
	if err := s.snap.Persist(); err != nil {
		return err
	}
	if err := netlink.RuleAdd(rule); err != nil {
		return fmt.Errorf("add ip rule pri=%d: %w", s.cfg.RoutingRulePriority, err)
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
