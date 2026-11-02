package main

import (
	"net"
	"testing"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

func TestIOUPort(t *testing.T) {
	cases := []struct {
		mgmtIdx, idx int
		want         byte
	}{
		{0, 0, 0x00},  // eth0 -> 0/0
		{0, 1, 0x10},  // eth1 -> 0/1
		{0, 3, 0x30},  // eth3 -> 0/3
		{0, 4, 0x01},  // eth4 -> 1/0
		{0, 63, 0x3f}, // eth63 -> 15/3
		{6, 0, 0x21},  // mgmt Ethernet1/2: eth0 -> 1/2
		{6, 6, 0x00},  // eth6 -> 0/0
		{6, 1, 0x10},  // others unchanged
	}
	for _, c := range cases {
		b := &bridge{mgmtIdx: c.mgmtIdx}
		if got := b.iouPort(c.idx); got != c.want {
			t.Errorf("mgmt=%d eth%d: got %#x, want %#x", c.mgmtIdx, c.idx, got, c.want)
		}
	}
}

func TestNumSlots(t *testing.T) {
	cases := []struct {
		env     string
		mgmtIdx int
		idxs    []int
		want    int
	}{
		{"", 0, nil, 1},         // mgmt only
		{"", 0, []int{1, 3}, 1}, // eth1, eth3 in slot 0
		{"", 0, []int{1, 4}, 2}, // eth4 -> 1/0
		{"", 6, nil, 2},         // mgmt Ethernet1/2 needs slot 1
		{"", 6, []int{6}, 2},    // eth6 swaps to 0/0
		{"", 0, []int{1, 9}, 3}, // eth9 -> 2/1
		{"4", 0, []int{1}, 4},   // CLAB_IOL_NUM_SLOTS wins
	}
	for _, c := range cases {
		t.Setenv("CLAB_IOL_NUM_SLOTS", c.env)
		b := &bridge{mgmtIdx: c.mgmtIdx}
		if got := b.numSlots(c.idxs); got != c.want {
			t.Errorf("env=%q mgmt=%d idxs=%v: got %d, want %d", c.env, c.mgmtIdx, c.idxs, got, c.want)
		}
	}
}

func TestNetmap(t *testing.T) {
	got := string(netmap(2, 513, 2))
	want := "2:0/0 513:0/0\n2:0/1 513:0/1\n2:0/2 513:0/2\n2:0/3 513:0/3\n" +
		"2:1/0 513:1/0\n2:1/1 513:1/1\n2:1/2 513:1/2\n2:1/3 513:1/3\n"
	if got != want {
		t.Errorf("got\n%swant\n%s", got, want)
	}
}

func TestTelnetKeys(t *testing.T) {
	cases := []struct {
		name string
		in   []string // chunks as read from the client
		want string
	}{
		{"plain", []string{"show ver"}, "show ver"},
		{"CR NUL", []string{"a\r\x00b"}, "a\rb"},
		{"CR LF", []string{"a\r\nb"}, "a\rb"},
		{"CR then CR", []string{"\r\r"}, "\r\r"},
		{"negotiation", []string{"\xff\xfd\x01a\xff\xfb\x03b"}, "ab"},
		{"subnegotiation", []string{"\xff\xfa\x18\x00xterm\xff\xf0a"}, "a"},
		{"escaped IAC", []string{"\xff\xff"}, "\xff"},
		{"split IAC", []string{"a\xff", "\xfd", "\x01b"}, "ab"},
		{"split CR NUL", []string{"a\r", "\x00b"}, "a\rb"},
	}
	for _, c := range cases {
		var f telnetFilter
		got := ""
		for _, chunk := range c.in {
			got += string(f.keys([]byte(chunk)))
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestIsConsole(t *testing.T) {
	eth := func(etype layers.EthernetType, ls ...gopacket.SerializableLayer) []byte {
		buf := gopacket.NewSerializeBuffer()
		opts := gopacket.SerializeOptions{FixLengths: true}
		ls = append([]gopacket.SerializableLayer{&layers.Ethernet{EthernetType: etype, SrcMAC: make(net.HardwareAddr, 6), DstMAC: make(net.HardwareAddr, 6)}}, ls...)
		if err := gopacket.SerializeLayers(buf, opts, ls...); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	ip4 := func(proto layers.IPProtocol, fragOffset uint16) *layers.IPv4 {
		return &layers.IPv4{Version: 4, IHL: 5, Protocol: proto, FragOffset: fragOffset,
			SrcIP: net.IPv4(10, 0, 0, 1), DstIP: net.IPv4(10, 0, 0, 2)}
	}
	v4, v4udp, v4frag := ip4(layers.IPProtocolTCP, 0), ip4(layers.IPProtocolUDP, 0), ip4(layers.IPProtocolTCP, 1)
	v6 := &layers.IPv6{Version: 6, NextHeader: layers.IPProtocolTCP, SrcIP: net.ParseIP("fd00::1"), DstIP: net.ParseIP("fd00::2")}
	tcp := func(port layers.TCPPort) *layers.TCP {
		return &layers.TCP{SrcPort: 40000, DstPort: port, DataOffset: 5}
	}
	cases := []struct {
		name  string
		frame []byte
		want  bool
	}{
		{"IPv4 TCP 5000", eth(layers.EthernetTypeIPv4, v4, tcp(5000)), true},
		{"IPv4 TCP 22", eth(layers.EthernetTypeIPv4, v4, tcp(22)), false},
		{"IPv4 UDP 5000", eth(layers.EthernetTypeIPv4, v4udp, &layers.UDP{SrcPort: 40000, DstPort: 5000}), false},
		{"IPv4 later fragment", eth(layers.EthernetTypeIPv4, v4frag, tcp(5000)), false},
		{"IPv6 TCP 5000", eth(layers.EthernetTypeIPv6, v6, tcp(5000)), true},
		{"IPv6 TCP 23", eth(layers.EthernetTypeIPv6, v6, tcp(23)), false},
		{"ARP", eth(layers.EthernetTypeARP, &layers.ARP{AddrType: layers.LinkTypeEthernet, Protocol: layers.EthernetTypeIPv4,
			HwAddressSize: 6, ProtAddressSize: 4, SourceHwAddress: make([]byte, 6), SourceProtAddress: make([]byte, 4),
			DstHwAddress: make([]byte, 6), DstProtAddress: make([]byte, 4)}), false},
		{"IPv4 no header", eth(layers.EthernetTypeIPv4), false},
		{"short", []byte{1, 2, 3}, false},
	}
	for _, c := range cases {
		if got := isConsole(c.frame); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
