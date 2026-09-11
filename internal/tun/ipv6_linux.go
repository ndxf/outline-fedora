//go:build linux

package tun

import (
	"fmt"
	"os"
	"strings"
)

// readIPv6Disabled reads /proc/sys/net/ipv6/conf/all/disable_ipv6 and
// returns "0" or "1" for round-tripping into the snapshot.
func readIPv6Disabled() (string, error) {
	b, err := os.ReadFile(IPv6DisableProcFile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", IPv6DisableProcFile, err)
	}
	v := strings.TrimSpace(string(b))
	if v != "0" && v != "1" {
		return "", fmt.Errorf("unexpected ipv6 disable value %q", v)
	}
	return v, nil
}

// writeIPv6Disabled writes "0" or "1" to the sysctl.
func writeIPv6Disabled(v string) error {
	if v != "0" && v != "1" {
		return fmt.Errorf("value must be 0 or 1, got %q", v)
	}
	return os.WriteFile(IPv6DisableProcFile, []byte(v), 0o644)
}
