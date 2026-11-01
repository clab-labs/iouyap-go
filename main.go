// Command iouyap bridges a Cisco IOL instance to the container's ethN
// interfaces. Interfaces are discovered through netlink and attached or
// detached as they appear and disappear; no iouyap.ini or NETMAP is read.
//
// Interface ethN maps to IOL Ethernet slot/port with slot = N/4 and
// port = N%4. When CLAB_IOL_MGMT_INTF names another port (for example
// Ethernet1/2), eth0 and that port swap places, as containerlab does.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	version = "1.0.0"
	hdrSize = 8
	maxMTU  = 0x1000
	vlanLen = 4
	maxIdx  = 64 // 16 slots of 4 ports

	// levelTrace logs every packet.
	levelTrace = slog.LevelDebug - 4
)

var (
	ctx       = context.Background()
	ethRe     = regexp.MustCompile(`^eth(\d+)$`)
	mgmtIntRe = regexp.MustCompile(`(\d+)/(\d+)$`)
)

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// bridge owns the IOU socket and every attached port.
type bridge struct {
	uds     *net.UnixConn
	dir     string
	id      int          // IOU ID of this bridge (513 under containerlab)
	iolID   atomic.Int32 // IOL instance ID, learned from received frames
	mgmtIdx int

	mu     sync.Mutex
	byName map[string]*ethPort
	byPort [256]atomic.Pointer[ethPort]
}

// ethPort is one IOL port bridged to one Ethernet device.
type ethPort struct {
	b       *bridge
	dev     string
	port    byte // IOU port byte: unit<<4 | bay
	fd      int
	ifindex int
	log     *slog.Logger
	lastErr string // last send error to IOL, owned by forward

	// mu and closed keep send from writing to fd after run closes it
	// and the number is reused.
	mu     sync.RWMutex
	closed bool

	rx, tx, drops atomic.Uint64
}

// iouPort maps a Linux ethN index to the IOU port byte.
func (b *bridge) iouPort(idx int) byte {
	switch idx {
	case 0:
		idx = b.mgmtIdx
	case b.mgmtIdx:
		idx = 0
	}
	slot, port := idx/4, idx%4
	return byte(port<<4 | slot)
}

// openEth opens a promiscuous raw socket on dev that reports stripped VLAN tags.
func openEth(dev string) (fd, ifindex int, err error) {
	ifi, err := net.InterfaceByName(dev)
	if err != nil {
		return -1, 0, err
	}
	fd, err = unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		return -1, 0, err
	}
	sa := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: ifi.Index}
	if err = unix.Bind(fd, sa); err == nil {
		err = unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP,
			&unix.PacketMreq{Ifindex: int32(ifi.Index), Type: unix.PACKET_MR_PROMISC})
	}
	if err == nil {
		err = unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_AUXDATA, 1)
	}
	if err != nil {
		unix.Close(fd)
		return -1, 0, err
	}
	return fd, ifi.Index, nil
}

// scan attaches every ethN interface that is not attached yet.
func (b *bridge) scan() {
	ifs, err := net.Interfaces()
	if err != nil {
		slog.Error("list interfaces failed", "err", err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ifi := range ifs {
		m := ethRe.FindStringSubmatch(ifi.Name)
		if m == nil || b.byName[ifi.Name] != nil {
			continue
		}
		idx, _ := strconv.Atoi(m[1])
		if idx >= maxIdx {
			slog.Warn("interface index too high, ignoring", "dev", ifi.Name)
			continue
		}
		port := b.iouPort(idx)
		log := slog.With("dev", ifi.Name, "port", fmt.Sprintf("%d/%d", port&0x0f, port>>4))
		fd, ifindex, err := openEth(ifi.Name)
		if err != nil {
			log.Warn("attach failed", "err", err)
			continue
		}
		p := &ethPort{b: b, dev: ifi.Name, port: port, fd: fd, ifindex: ifindex, log: log}
		b.byName[p.dev] = p
		b.byPort[port].Store(p)
		log.Info("interface attached", "ifindex", ifindex)
		go p.run()
	}
}

// watch rescans interfaces on every netlink link event.
func (b *bridge) watch() {
	b.scan()
	nl, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err == nil {
		err = unix.Bind(nl, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: unix.RTMGRP_LINK})
	}
	if err != nil {
		fatal("netlink subscribe failed", "err", err)
	}
	buf := make([]byte, 1<<16)
	for {
		// Message content is irrelevant: any link change triggers a rescan.
		// ENOBUFS means events were lost, which a rescan also covers.
		if _, err := unix.Read(nl, buf); err != nil && !errors.Is(err, unix.ENOBUFS) && !errors.Is(err, unix.EINTR) {
			slog.Error("netlink read failed", "err", err)
			os.Exit(1)
		}
		b.scan()
	}
}

// run forwards frames until the device disappears, then detaches the port.
func (p *ethPort) run() {
	reason := p.forward()

	b := p.b
	b.mu.Lock()
	delete(b.byName, p.dev)
	b.byPort[p.port].CompareAndSwap(p, nil)
	b.mu.Unlock()

	p.mu.Lock()
	p.closed = true
	unix.Close(p.fd)
	p.mu.Unlock()
	p.log.Warn("interface detached", "reason", reason,
		"rx", p.rx.Load(), "tx", p.tx.Load(), "drops", p.drops.Load())

	// The device may have been recreated before this port let go of its name.
	b.scan()
}

// recvEth reads one frame into buf, re-inserting the 802.1Q tag the kernel stripped.
func recvEth(fd int, buf, oob []byte) (int, error) {
	n, oobn, _, _, err := unix.Recvmsg(fd, buf[:len(buf)-vlanLen], oob, unix.MSG_TRUNC)
	if err != nil || n <= 0 {
		return n, err
	}
	if n > len(buf)-vlanLen {
		n = len(buf) - vlanLen
	}
	msgs, _ := unix.ParseSocketControlMessage(oob[:oobn])
	for _, m := range msgs {
		if m.Header.Level != unix.SOL_PACKET || m.Header.Type != unix.PACKET_AUXDATA ||
			len(m.Data) < int(unsafe.Sizeof(unix.TpacketAuxdata{})) {
			continue
		}
		aux := (*unix.TpacketAuxdata)(unsafe.Pointer(&m.Data[0]))
		if aux.Vlan_tci == 0 && aux.Status&unix.TP_STATUS_VLAN_VALID == 0 {
			continue
		}
		tpid := uint16(unix.ETH_P_8021Q)
		if aux.Vlan_tpid != 0 || aux.Status&unix.TP_STATUS_VLAN_TPID_VALID != 0 {
			tpid = aux.Vlan_tpid
		}
		copy(buf[12+vlanLen:n+vlanLen], buf[12:n])
		binary.BigEndian.PutUint16(buf[12:], tpid)
		binary.BigEndian.PutUint16(buf[14:], aux.Vlan_tci)
		n += vlanLen
	}
	return n, nil
}

// forward copies frames from the device to IOL until the device goes away.
func (p *ethPort) forward() error {
	buf := make([]byte, hdrSize+maxMTU)
	oob := make([]byte, unix.CmsgSpace(int(unsafe.Sizeof(unix.TpacketAuxdata{}))))
	for {
		n, err := recvEth(p.fd, buf[hdrSize:], oob)
		switch {
		case err == nil:
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ENETDOWN):
			// Kernel reports admin down and device removal the same way.
			if ifi, e := net.InterfaceByName(p.dev); e == nil && ifi.Index == p.ifindex {
				p.log.Info("interface down")
				continue
			}
			return errors.New("interface removed")
		default:
			return err
		}
		p.rx.Add(1)
		p.log.Log(ctx, levelTrace, "eth to iol", "bytes", n)

		iol := int(p.b.iolID.Load())
		binary.BigEndian.PutUint16(buf[0:], uint16(iol))
		binary.BigEndian.PutUint16(buf[2:], uint16(p.b.id))
		buf[4], buf[5], buf[6], buf[7] = p.port, p.port, 1, 0
		dst := &net.UnixAddr{Name: filepath.Join(p.b.dir, strconv.Itoa(iol)), Net: "unixgram"}
		_, err = p.b.uds.WriteToUnix(buf[:hdrSize+n], dst)
		p.sendResult(dst.Name, err)
	}
}

// sendResult logs changes in IOL reachability instead of every failure.
func (p *ethPort) sendResult(dst string, err error) {
	msg := ""
	if err != nil {
		p.drops.Add(1)
		msg = err.Error()
	}
	if msg == p.lastErr {
		return
	}
	p.lastErr = msg
	switch {
	case err == nil:
		p.log.Info("IOL reachable", "socket", dst)
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ECONNREFUSED):
		p.log.Info("IOL not listening, dropping frames", "socket", dst)
	default:
		p.log.Warn("send to IOL failed", "socket", dst, "err", err)
	}
}

// send writes one frame from IOL to the device.
func (p *ethPort) send(frame []byte) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		p.drops.Add(1)
		return
	}
	sa := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: p.ifindex}
	if err := unix.Sendto(p.fd, frame, 0, sa); err != nil {
		p.drops.Add(1)
		p.log.Debug("eth write failed", "err", err)
		return
	}
	p.tx.Add(1)
	p.log.Log(ctx, levelTrace, "iol to eth", "bytes", len(frame))
}

// iolToEth forwards frames from IOL to the device of the addressed port.
func (b *bridge) iolToEth() {
	buf := make([]byte, hdrSize+maxMTU)
	for {
		n, _, err := b.uds.ReadFromUnix(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Error("IOU socket read failed", "err", err)
			os.Exit(1)
		}
		if n <= hdrSize {
			continue
		}
		if src := int32(binary.BigEndian.Uint16(buf[2:]) & 0x7ff); src != b.iolID.Load() {
			slog.Info("IOL ID learned from traffic", "old", b.iolID.Load(), "new", src)
			b.iolID.Store(src)
		}
		p := b.byPort[buf[4]].Load()
		if p == nil {
			slog.Log(ctx, levelTrace, "drop: no interface for port",
				"port", fmt.Sprintf("%d/%d", buf[4]&0x0f, buf[4]>>4))
			continue
		}
		p.send(buf[hdrSize:n])
	}
}

func (b *bridge) stats() {
	b.mu.Lock()
	defer b.mu.Unlock()
	slog.Info("stats", "iol_id", b.iolID.Load(), "ports", len(b.byName))
	for _, p := range b.byName {
		p.log.Info("stats", "rx", p.rx.Load(), "tx", p.tx.Load(), "drops", p.drops.Load())
	}
}

// lock takes an fcntl write lock on name.lck and writes the process PID into it.
func lock(name string) (*os.File, error) {
	f, err := os.OpenFile(name+".lck", os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	fl := unix.Flock_t{Type: unix.F_WRLCK}
	if err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &fl); err != nil {
		unix.FcntlFlock(f.Fd(), unix.F_GETLK, &fl)
		f.Close()
		return nil, fmt.Errorf("PID %d already has a lock on %s", fl.Pid, name)
	}
	f.Truncate(0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return f, nil
}

func usage() {
	fmt.Printf(`Usage: %s [OPTION]... [ID]

Bridge a Cisco IOL instance to the container's ethN interfaces.
ID is our IOU ID (default 513).

Options:
  -h        print this message and exit
  -q        only log warnings and errors
  -v        debug logging; -vv also logs every packet
  -p IOL_ID IOL instance ID (default $IOL_PID, else 1; also learned from traffic)
  -V        print version and exit

Environment:
  IOL_PID             IOL instance ID
  CLAB_IOL_MGMT_INTF  IOL port backed by eth0, e.g. Ethernet1/2 (default Ethernet0/0)

Send SIGUSR1 to log per-port counters.
`, filepath.Base(os.Args[0]))
}

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

func main() {
	level := slog.LevelInfo
	iolArg := os.Getenv("IOL_PID")
	// Parsed by hand because entrypoints pass flags after the ID
	// ("iouyap 513 -q"), where package flag stops parsing.
	var pos []string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 2 || a[0] != '-' {
			pos = append(pos, a)
			continue
		}
		for j := 1; j < len(a); j++ {
			switch c := a[j]; c {
			case 'h':
				usage()
				return
			case 'V':
				fmt.Println("iouyap version", version)
				return
			case 'q':
				level = slog.LevelWarn
			case 'v':
				level -= 4
			case 'p':
				iolArg = a[j+1:]
				if iolArg == "" {
					if i++; i >= len(args) {
						fmt.Fprintln(os.Stderr, "option -p needs an argument")
						os.Exit(1)
					}
					iolArg = args[i]
				}
				j = len(a)
			default:
				fmt.Fprintf(os.Stderr, "unknown option -%c; use -h for help\n", c)
				os.Exit(1)
			}
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey && a.Value.Any().(slog.Level) == levelTrace {
				a.Value = slog.StringValue("TRACE")
			}
			return a
		},
	})))

	b := &bridge{byName: map[string]*ethPort{}, id: 513}
	if len(pos) > 1 {
		fatal("invalid args; use -h for help")
	}
	var err error
	if len(pos) == 1 {
		if b.id, err = strconv.Atoi(pos[0]); err != nil || b.id < 1 || b.id > 1024 {
			fatal("bad ID", "id", pos[0])
		}
	}
	iolID := 1
	if iolArg != "" {
		if iolID, err = strconv.Atoi(iolArg); err != nil || iolID < 1 || iolID > 1024 {
			fatal("bad IOL ID", "iol_id", iolArg)
		}
	}
	b.iolID.Store(int32(iolID))
	if v := os.Getenv("CLAB_IOL_MGMT_INTF"); v != "" {
		m := mgmtIntRe.FindStringSubmatch(v)
		if m == nil {
			fatal("bad CLAB_IOL_MGMT_INTF", "value", v)
		}
		slot, _ := strconv.Atoi(m[1])
		port, _ := strconv.Atoi(m[2])
		if slot > 15 || port > 3 {
			fatal("CLAB_IOL_MGMT_INTF out of range", "value", v)
		}
		b.mgmtIdx = slot*4 + port
	}
	slog.Info("starting", "version", version, "id", b.id, "iol_id", iolID,
		"mgmt_port", fmt.Sprintf("%d/%d", b.mgmtIdx/4, b.mgmtIdx%4))

	b.dir = fmt.Sprintf("/tmp/netio%d", os.Getuid())
	os.Mkdir(b.dir, 0o700)
	sockName := filepath.Join(b.dir, strconv.Itoa(b.id))
	lf, err := lock(sockName)
	if err != nil {
		fatal("cannot lock socket", "err", err)
	}
	os.Remove(sockName)
	b.uds, err = net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockName, Net: "unixgram"})
	if err != nil {
		fatal("cannot open IOU socket", "err", err)
	}

	go b.iolToEth()
	go b.watch()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	for s := range sig {
		if s == syscall.SIGUSR1 {
			b.stats()
			continue
		}
		slog.Info("exiting", "signal", s.String())
		break
	}
	b.uds.Close()
	os.Remove(sockName)
	os.Remove(lf.Name())
	lf.Close()
}
