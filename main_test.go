package main

import "testing"

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
