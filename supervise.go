package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// supervise starts IOL once containerlab has attached every data interface,
// and runs it until it exits. It returns the exit code for iouyap.
//
// The container's own stack shares the management address with IOL, but
// only to serve the console; see shareMgmt.
//
// IOL reads NETMAP from its working directory once, at boot, and only
// transmits on the ports listed there, so NETMAP lists every port of every
// slot. The slot count is also fixed at boot, and is passed to IOL as -e.
//
// IOL runs on its own PTY, which is shared by the container TTY and telnet
// clients on consolePort.
func (b *bridge) supervise(argv []string) int {
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)

	if err := b.shareMgmt(); err != nil {
		slog.Warn("console limited to the container, management address not shared", "err", err)
		if err := flushAddrs("eth0"); err != nil {
			slog.Error("cannot remove eth0 addresses", "err", err)
			return 1
		}
	}
	idxs, ok := b.waitIntfs(sig)
	if !ok {
		return 0
	}
	slots := b.numSlots(idxs)
	if err := os.WriteFile("NETMAP", netmap(int(b.iolID.Load()), b.id, slots), 0o644); err != nil {
		slog.Error("cannot write NETMAP", "err", err)
		return 1
	}

	ptmx, tty, err := pty.Open()
	if err == nil {
		err = noSignalKeys(tty)
	}
	if err != nil {
		slog.Error("cannot open PTY", "err", err)
		return 1
	}
	cmd := exec.Command(argv[0], append(argv[1:], "-e", strconv.Itoa(slots))...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	// IOL gets its own session with the PTY as controlling terminal, so no
	// terminal signal reaches iouyap.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	err = cmd.Start()
	// IOL holds the only slave descriptor, so reads from ptmx fail once it exits.
	tty.Close()
	if err != nil {
		slog.Error("cannot start IOL", "err", err)
		return 1
	}
	slog.Info("IOL started", "pid", cmd.Process.Pid, "slots", slots, "interfaces", len(idxs))
	defer rawStdin()()
	con := &console{pty: ptmx, clients: map[net.Conn]bool{}}
	go con.pump()
	go con.input()
	go con.serve()

	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	for {
		select {
		case s := <-sig:
			switch s {
			case syscall.SIGTERM, syscall.SIGINT:
				slog.Info("stopping IOL", "signal", s.String())
				cmd.Process.Signal(s)
			case syscall.SIGUSR1:
				b.stats()
			}
		case <-done:
			slog.Warn("IOL exited", "state", cmd.ProcessState.String())
			// Shell convention: 128+N when IOL is killed by signal N.
			if ws := cmd.ProcessState.Sys().(syscall.WaitStatus); ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return cmd.ProcessState.ExitCode()
		}
	}
}

// waitIntfs polls until CLAB_INTFS data interfaces exist and returns their
// indexes. It returns false if SIGTERM or SIGINT arrives first.
func (b *bridge) waitIntfs(sig <-chan os.Signal) ([]int, bool) {
	want := 0
	if v := os.Getenv("CLAB_INTFS"); v != "" {
		var err error
		if want, err = strconv.Atoi(v); err != nil || want < 0 {
			fatal("bad CLAB_INTFS", "value", v)
		}
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var logged time.Time
	for {
		if idxs := dataIdxs(); len(idxs) >= want {
			return idxs, true
		} else if time.Since(logged) > 10*time.Second {
			slog.Info("waiting for interfaces", "have", len(idxs), "want", want)
			logged = time.Now()
		}
		select {
		case s := <-sig:
			switch s {
			case syscall.SIGTERM, syscall.SIGINT:
				slog.Info("exiting before IOL start", "signal", s.String())
				return nil, false
			case syscall.SIGUSR1:
				b.stats()
			}
		case <-tick.C:
		}
	}
}

// dataIdxs returns the indexes of the ethN interfaces other than eth0.
func dataIdxs() []int {
	devs, err := ethDevs()
	if err != nil {
		fatal("list interfaces failed", "err", err)
	}
	var idxs []int
	for _, idx := range devs {
		if idx > 0 && idx < maxIdx {
			idxs = append(idxs, idx)
		}
	}
	return idxs
}

// numSlots returns CLAB_IOL_NUM_SLOTS, or else the fewest slots that hold
// the management port and every interface in idxs.
func (b *bridge) numSlots(idxs []int) int {
	if v := os.Getenv("CLAB_IOL_NUM_SLOTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxIdx/4 {
			fatal("bad CLAB_IOL_NUM_SLOTS", "value", v)
		}
		return n
	}
	n := b.mgmtIdx/4 + 1
	for _, idx := range idxs {
		slot := int(b.iouPort(idx) & 0x0f)
		n = max(n, slot+1)
	}
	return n
}

// netmap maps every port of every slot of IOL instance iolID to bridge id.
func netmap(iolID, id, slots int) []byte {
	var out []byte
	for slot := range slots {
		for port := range 4 {
			out = fmt.Appendf(out, "%d:%d/%d %d:%d/%d\n", iolID, slot, port, id, slot, port)
		}
	}
	return out
}
