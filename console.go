package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// consolePort serves the IOL console over telnet on every container address,
// including the management address shared with IOL; see shareMgmt.
const consolePort = 5000

// Telnet bytes from RFC 854 and RFC 857/858.
const (
	telIAC  = 255
	telSB   = 250
	telSE   = 240
	telWILL = 251
	telEcho = 1
	telSGA  = 3
)

// console shares IOL's PTY between the container TTY and telnet clients.
type console struct {
	pty     *os.File
	mu      sync.Mutex
	clients map[net.Conn]bool
}

// noSignalKeys stops the PTY turning Ctrl-C, Ctrl-\ and Ctrl-Z into
// signals, so IOS receives them as keys. IOL leaves ISIG as it finds it,
// and a SIGINT stops IOL.
func noSignalKeys(tty *os.File) error {
	t, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		return err
	}
	t.Lflag &^= unix.ISIG
	return unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, t)
}

// rawStdin puts the container TTY in raw input mode, so keys reach IOL's
// PTY unechoed and unbuffered, and returns a function that restores it.
// Output processing stays on so iouyap's log lines keep their line breaks.
func rawStdin() (restore func()) {
	old, err := unix.IoctlGetTermios(0, unix.TCGETS)
	if err != nil {
		return func() {}
	}
	raw := *old
	raw.Iflag &^= unix.IXON | unix.ICRNL | unix.INLCR | unix.IGNCR | unix.ISTRIP | unix.BRKINT
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	unix.IoctlSetTermios(0, unix.TCSETS, &raw)
	return func() { unix.IoctlSetTermios(0, unix.TCSETS, old) }
}

// pump copies IOL output to the container TTY and every telnet client until
// the PTY fails, which happens once IOL exits.
func (c *console) pump() {
	buf := make([]byte, 4096)
	for {
		n, err := c.pty.Read(buf)
		if err != nil {
			return
		}
		os.Stdout.Write(buf[:n])
		out := bytes.ReplaceAll(buf[:n], []byte{telIAC}, []byte{telIAC, telIAC})
		c.mu.Lock()
		for conn := range c.clients {
			// A stalled client is dropped rather than stalling the console.
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := conn.Write(out); err != nil {
				delete(c.clients, conn)
				conn.Close()
			}
		}
		c.mu.Unlock()
	}
}

// serve accepts telnet clients on consolePort. The console is optional, so
// a listen failure is logged and IOL runs without it.
func (c *console) serve() {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", consolePort))
	if err != nil {
		slog.Warn("telnet console unavailable", "err", err)
		return
	}
	slog.Info("telnet console listening", "addr", ln.Addr().String())
	for {
		conn, err := ln.Accept()
		if err != nil {
			slog.Warn("telnet console stopped", "err", err)
			return
		}
		go c.client(conn)
	}
}

// client forwards keys from one telnet client to IOL until it disconnects.
func (c *console) client(conn net.Conn) {
	log := slog.With("client", conn.RemoteAddr().String())
	log.Info("console client connected")
	// Character mode: the server echoes, and the client sends each key as typed.
	conn.Write([]byte{telIAC, telWILL, telEcho, telIAC, telWILL, telSGA})
	c.mu.Lock()
	c.clients[conn] = true
	c.mu.Unlock()

	var f telnetFilter
	buf := make([]byte, 1024)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			break
		}
		c.pty.Write(f.keys(buf[:n]))
	}

	c.mu.Lock()
	delete(c.clients, conn)
	c.mu.Unlock()
	conn.Close()
	log.Info("console client disconnected")
}

// input copies keys typed on the container TTY to IOL.
func (c *console) input() {
	io.Copy(c.pty, os.Stdin)
}

// telnetFilter strips telnet protocol bytes from client input.
type telnetFilter struct {
	state byte
}

const (
	telData   = iota
	telCmd    // after IAC
	telOpt    // after IAC WILL/WONT/DO/DONT
	telSub    // inside IAC SB ... IAC SE
	telSubIAC // IAC inside a subnegotiation
	telCR     // after CR
)

// keys returns the typed bytes in in. It drops option negotiation and
// subnegotiation, unescapes IAC IAC, and drops the NUL or LF that a client
// sends after CR.
func (f *telnetFilter) keys(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for _, b := range in {
		switch f.state {
		case telCmd:
			switch {
			case b == telIAC:
				out = append(out, b)
				f.state = telData
			case b == telSB:
				f.state = telSub
			case b >= telWILL:
				f.state = telOpt
			default:
				f.state = telData
			}
		case telOpt:
			f.state = telData
		case telSub:
			if b == telIAC {
				f.state = telSubIAC
			}
		case telSubIAC:
			f.state = telSub
			if b == telSE {
				f.state = telData
			}
		default:
			if f.state == telCR && (b == 0 || b == '\n') {
				f.state = telData
				continue
			}
			f.state = telData
			switch b {
			case telIAC:
				f.state = telCmd
			case '\r':
				out = append(out, b)
				f.state = telCR
			default:
				out = append(out, b)
			}
		}
	}
	return out
}
