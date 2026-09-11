//go:build linux

package tun

import (
	"errors"
	"fmt"

	"github.com/songgao/water"
	"github.com/vishvananda/netlink"
)

// tunDevice bundles the water.Interface (the /dev/net/tun fd) with the
// netlink handle used to bring it up and address it.
type tunDevice struct {
	*water.Interface
	link netlink.Link
}

func newTunDevice(name, ip string) (*tunDevice, error) {
	if name == "" {
		return nil, errors.New("tun name required")
	}
	if ip == "" {
		return nil, errors.New("tun ip required")
	}

	iface, err := water.New(water.Config{
		DeviceType: water.TUN,
		PlatformSpecificParams: water.PlatformSpecificParams{
			Name:    name,
			Persist: false, // kernel GCs the tun when our fd closes
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create tun: %w", err)
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		iface.Close()
		return nil, fmt.Errorf("lookup new tun link %q: %w", name, err)
	}

	d := &tunDevice{Interface: iface, link: link}

	addr, err := netlink.ParseAddr(ip + "/32")
	if err != nil {
		iface.Close()
		return nil, fmt.Errorf("parse tun ip %q: %w", ip, err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		iface.Close()
		return nil, fmt.Errorf("assign %s to %s: %w", addr, name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		iface.Close()
		return nil, fmt.Errorf("link up %s: %w", name, err)
	}
	return d, nil
}
