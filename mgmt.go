package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/vishvananda/netlink"
)

// shareMgmt lets the container's stack serve the console on the management
// address that IOL owns. eth0 takes IOL's management MAC, so both stacks
// answer ARP and ND the same way, and a tc filter keeps the container's
// stack to the console port. iouyap reads frames before tc runs, so IOL
// still receives every frame except console traffic, which forward skips.
func (b *bridge) shareMgmt() error {
	// IOL derives a port's MAC from its ID and port byte; followMgmtMAC
	// corrects eth0 if IOL uses another.
	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, byte(b.iolID.Load()), b.iouPort(0)}
	if err := setMAC("eth0", mac); err != nil {
		return fmt.Errorf("set eth0 MAC: %w", err)
	}
	if err := tc("qdisc", "replace", "dev", "eth0", "clsact"); err != nil {
		return err
	}
	port := strconv.Itoa(consolePort)
	// The container's stack accepts the console port, ARP (never matched,
	// so passed), and IPv6 neighbour discovery; other IP traffic is IOL's.
	filters := [][]string{
		{"ip", "flower", "ip_proto", "tcp", "dst_port", port, "action", "pass"},
		{"ipv6", "flower", "ip_proto", "tcp", "dst_port", port, "action", "pass"},
		{"ipv6", "flower", "ip_proto", "icmpv6", "type", "135", "action", "pass"},
		{"ipv6", "flower", "ip_proto", "icmpv6", "type", "136", "action", "pass"},
		{"ip", "flower", "action", "drop"},
		{"ipv6", "flower", "action", "drop"},
	}
	for i, f := range filters {
		args := append([]string{"filter", "replace", "dev", "eth0", "ingress", "pref", strconv.Itoa(i + 1), "protocol"}, f...)
		if err := tc(args...); err != nil {
			tc("qdisc", "del", "dev", "eth0", "clsact")
			return err
		}
	}
	b.mgmtMAC.Store(&mac)
	slog.Info("management address shared for the console", "mac", mac.String(), "port", consolePort)
	return nil
}

// tc runs the tc command with args.
func tc(args ...string) error {
	if out, err := exec.Command("tc", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tc %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// followMgmtMAC keeps eth0's MAC equal to src, the MAC IOL sends with on
// the management port, while the management address is shared.
func (b *bridge) followMgmtMAC(src net.HardwareAddr) {
	cur := b.mgmtMAC.Load()
	if cur == nil || bytes.Equal(*cur, src) {
		return
	}
	// The MAC is stored even if setting it fails, so the failure is logged
	// once rather than for every frame.
	mac := slices.Clone(src)
	b.mgmtMAC.Store(&mac)
	if err := setMAC("eth0", mac); err != nil {
		slog.Warn("cannot follow IOL management MAC", "mac", mac.String(), "err", err)
		return
	}
	slog.Info("eth0 MAC set to IOL's", "mac", mac.String())
}

// isConsole reports whether frame carries TCP to consolePort.
func isConsole(frame []byte) bool {
	pkt := gopacket.NewPacket(frame, layers.LayerTypeEthernet, gopacket.DecodeOptions{Lazy: true, NoCopy: true})
	tcp, ok := pkt.Layer(layers.LayerTypeTCP).(*layers.TCP)
	return ok && tcp.DstPort == consolePort
}

// flushAddrs removes every IPv4 and IPv6 address from dev.
func flushAddrs(dev string) error {
	link, err := netlink.LinkByName(dev)
	if err != nil {
		return err
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if err := netlink.AddrDel(link, &a); err != nil {
			return fmt.Errorf("delete %s: %w", a.IPNet, err)
		}
		slog.Info("address removed", "dev", dev, "addr", a.IPNet.String())
	}
	return nil
}

// setMAC sets the hardware address of dev.
func setMAC(dev string, mac net.HardwareAddr) error {
	link, err := netlink.LinkByName(dev)
	if err != nil {
		return err
	}
	return netlink.LinkSetHardwareAddr(link, mac)
}
