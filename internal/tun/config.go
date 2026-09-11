// Package tun owns a single active VPN session: TUN device, routes,
// ip rule, DNS override, IPv6 suppression, and the tun<->Shadowsocks
// packet bridge. Every host-mutating action is recorded in a snapshot
// BEFORE it happens, so ouf-panic can revert even if this package
// crashes mid-flight.
//
// Linux-only. Other platforms are stubbed via build tags.
package tun

import "fmt"

const (
	// DefaultTunName must be <= 15 chars (IFNAMSIZ - 1).
	DefaultTunName             = "ouftun0"
	DefaultTunIP               = "10.233.233.1"
	DefaultTunGatewayCIDR      = "10.233.233.2/32"
	DefaultTunMTU              = 1500
	DefaultRoutingTableID      = 233
	DefaultRoutingRulePriority = 23333
	DefaultDNSServerIP         = "9.9.9.9"

	IPv6DisableSysctlKey = "net.ipv6.conf.all.disable_ipv6"
	IPv6DisableProcFile  = "/proc/sys/net/ipv6/conf/all/disable_ipv6"
)

// Config controls a single connect attempt. Zero-values are filled in
// from the Default* constants above via ApplyDefaults.
type Config struct {
	TunName             string
	TunIP               string
	TunGatewayCIDR      string
	TunMTU              int
	RoutingTableID      int
	RoutingRulePriority int
	DNSServerIP         string

	// TransportURL is the ss:// URL for the remote Outline server.
	TransportURL string

	// ResolvPath is where the resolver config lives. Overridable for
	// tests; production always uses /etc/resolv.conf.
	ResolvPath string

	// StateDir is where the snapshot + backups live. Overridable for
	// tests; production always uses /var/lib/outline-fedora.
	StateDir string

	// OUFVersion is recorded in the snapshot's created_by field.
	OUFVersion string
}

func (c *Config) ApplyDefaults() {
	if c.TunName == "" {
		c.TunName = DefaultTunName
	}
	if c.TunIP == "" {
		c.TunIP = DefaultTunIP
	}
	if c.TunGatewayCIDR == "" {
		c.TunGatewayCIDR = DefaultTunGatewayCIDR
	}
	if c.TunMTU == 0 {
		c.TunMTU = DefaultTunMTU
	}
	if c.RoutingTableID == 0 {
		c.RoutingTableID = DefaultRoutingTableID
	}
	if c.RoutingRulePriority == 0 {
		c.RoutingRulePriority = DefaultRoutingRulePriority
	}
	if c.DNSServerIP == "" {
		c.DNSServerIP = DefaultDNSServerIP
	}
	if c.ResolvPath == "" {
		c.ResolvPath = "/etc/resolv.conf"
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/outline-fedora"
	}
	if c.OUFVersion == "" {
		c.OUFVersion = "dev"
	}
}

// Validate enforces the "our stuff only" bounds that the panic script
// also enforces. Keep in lockstep with the checks in scripts/ouf-panic.
func (c *Config) Validate() error {
	if len(c.TunName) == 0 || len(c.TunName) > 15 {
		return fmt.Errorf("tun name must be 1..15 chars, got %q", c.TunName)
	}
	switch c.TunName {
	case "", "lo":
		return fmt.Errorf("refusing reserved tun name %q", c.TunName)
	}
	if c.RoutingTableID <= 255 {
		return fmt.Errorf("routing table id must be >255 (reserved), got %d", c.RoutingTableID)
	}
	if c.RoutingRulePriority < 1000 {
		return fmt.Errorf("routing rule priority must be >=1000, got %d", c.RoutingRulePriority)
	}
	if c.TransportURL == "" {
		return fmt.Errorf("transport url required")
	}
	return nil
}
