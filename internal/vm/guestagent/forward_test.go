// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The mode takes a port and nothing else: every other way to name a destination is refused before any
// dial.
func TestForwardRefusesAnythingButAPort(t *testing.T) {
	for _, c := range []struct{ name, arg string }{
		{"host and port", "127.0.0.1:80"},
		{"host only", "127.0.0.1"},
		{"ipv6 loopback", "::1"},
		{"ipv6 loopback in brackets", "[::1]:80"},
		{"another loopback", "127.0.0.2"},
		{"another loopback and port", "127.0.0.2:80"},
		{"v4-mapped loopback", "::ffff:127.0.0.1"},
		{"all interfaces", "0.0.0.0"},
		{"all interfaces and port", "0.0.0.0:80"},
		{"name", "localhost"},
		{"name that resolves elsewhere", "example.com"},
		{"name and port", "example.com:80"},
		{"zero", "0"},
		{"too large", "65536"},
		{"far too large", "99999999999999999999"},
		{"negative", "-1"},
		{"signed", "+80"},
		{"hex", "0x50"},
		{"non-numeric", "http"},
		{"padded", " 80"},
		{"empty", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if p, err := ParseForwardPort(c.arg); err == nil {
				t.Fatalf("ParseForwardPort(%q) = %d, want a refusal", c.arg, p)
			}
			var errw bytes.Buffer
			if code := ForwardMain(context.Background(), []string{c.arg}, strings.NewReader(""), io.Discard, &errw); code != 2 {
				t.Fatalf("ForwardMain(%q) = %d (%s), want 2", c.arg, code, errw.String())
			}
		})
	}
	// A second argument (a host after the port, a flag, a probe flag without a port) is refused too.
	for _, args := range [][]string{nil, {"80", "10.0.0.1"}, {"80", "80"}, {"-probe"}, {"-probe", "80", "x"}, {"-x", "80"}} {
		if code := ForwardMain(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard); code != 2 {
			t.Errorf("ForwardMain(%q) = %d, want 2", args, code)
		}
	}
	for _, ok := range []string{"1", "80", "65535"} {
		if _, err := ParseForwardPort(ok); err != nil {
			t.Errorf("ParseForwardPort(%q): %v", ok, err)
		}
	}
}

// Whatever the port, the dial is the literal tcp4 127.0.0.1:<port>: nothing is resolved.
func TestForwardDialsLoopbackOnly(t *testing.T) {
	for _, port := range []int{1, 80, 8080, 65535} {
		var network, addr string
		dial := func(_ context.Context, n, a string) (net.Conn, error) {
			network, addr = n, a
			return nil, io.ErrClosedPipe
		}
		if err := forward(context.Background(), dial, port, strings.NewReader(""), io.Discard); err == nil {
			t.Fatalf("port %d: want the dial error", port)
		}
		if network != "tcp4" || addr != "127.0.0.1:"+strconv.Itoa(port) {
			t.Errorf("port %d dialed %s %s, want tcp4 127.0.0.1:%d", port, network, addr, port)
		}
	}
	argv := ForwardArgv(LinuxAgentPath, 8080)
	if got := strings.Join(argv, " "); got != "/usr/local/bin/wardyn-guest-agent forward 8080" {
		t.Errorf("ForwardArgv = %q", got)
	}
}

// A listener that is not on 127.0.0.1 is not reached by the same port number.
func TestForwardDoesNotReachOtherLoopbacks(t *testing.T) {
	for _, bind := range []string{"[::1]:0", "127.0.0.2:0"} {
		ln, err := net.Listen("tcp", bind)
		if err != nil {
			t.Logf("cannot bind %s here: %v", bind, err)
			continue
		}
		defer ln.Close()
		go func() {
			if c, err := ln.Accept(); err == nil {
				_, _ = c.Write([]byte("reached"))
				c.Close()
			}
		}()
		port := ln.Addr().(*net.TCPAddr).Port
		var out bytes.Buffer
		// Free the port on 127.0.0.1 can't be assumed, so only a listener-free port is meaningful:
		// bind 127.0.0.1 on the same number to check it is free first.
		probe, perr := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
		if perr != nil {
			t.Logf("127.0.0.1:%d is taken here: %v", port, perr)
			continue
		}
		probe.Close()
		if err := Forward(context.Background(), port, strings.NewReader(""), &out); err == nil || out.Len() != 0 {
			t.Errorf("%s: Forward reached it (err %v, out %q)", bind, err, out.String())
		}
	}
}

func echoLoopback(t *testing.T, reply string) (port int, got <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c) // until the client half-closes
		ch <- string(b)
		_, _ = c.Write([]byte(reply))
	}()
	return ln.Addr().(*net.TCPAddr).Port, ch
}

// Both directions carry bytes; the end of stdin half-closes, so a reply sent after the request still
// arrives.
func TestForwardCopiesBothWays(t *testing.T) {
	port, got := echoLoopback(t, "pong")
	var out bytes.Buffer
	if err := Forward(context.Background(), port, strings.NewReader("ping"), &out); err != nil {
		t.Fatal(err)
	}
	if g := <-got; g != "ping" {
		t.Errorf("the listener read %q, want ping", g)
	}
	if out.String() != "pong" {
		t.Errorf("stdout = %q, want pong", out.String())
	}
}

// The listener closing ends the mode even while stdin is still open.
func TestForwardCloseFromListenerEnds(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_, _ = c.Write([]byte("bye"))
			c.Close()
		}
	}()
	pr, pw := io.Pipe()
	defer pw.Close()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- Forward(context.Background(), ln.Addr().(*net.TCPAddr).Port, pr, &out) }()
	select {
	case err := <-done:
		if err != nil || out.String() != "bye" {
			t.Errorf("Forward = %v, out %q", err, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Forward did not end when the listener closed")
	}
}

// A cancelled context closes the connection and ends the mode.
func TestForwardCancelEnds(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}
	}()
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Forward(ctx, ln.Addr().(*net.TCPAddr).Port, pr, io.Discard) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("want the context error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Forward did not end on cancel")
	}
}

// With nothing listening the mode exits nonzero, and the probe says so by its exit code alone.
func TestForwardProbeAndRefusedDial(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ps := strconv.Itoa(port)
	if code := ForwardMain(context.Background(), []string{"-probe", ps}, nil, io.Discard, io.Discard); code != 0 {
		t.Errorf("probe of a listener = %d, want 0", code)
	}
	ln.Close()
	closed := "1" // nothing listens there; the freed port could be taken by another process
	if code := ForwardMain(context.Background(), []string{"-probe", closed}, nil, io.Discard, io.Discard); code != 1 {
		t.Errorf("probe of a closed port = %d, want 1", code)
	}
	if code := ForwardMain(context.Background(), []string{closed}, strings.NewReader(""), io.Discard, io.Discard); code != 1 {
		t.Errorf("forward to a closed port = %d, want 1", code)
	}
}

// The macOS guest's Screen Sharing ports (5900-5909) are refused on darwin and only there.
func TestForwardRefusesMacScreenSharingPorts(t *testing.T) {
	for _, c := range []struct {
		goos string
		arg  string
		ok   bool
	}{
		{"darwin", "5899", true},
		{"darwin", "5900", false},
		{"darwin", "5905", false},
		{"darwin", "5909", false},
		{"darwin", "5910", true},
		{"darwin", "8080", true},
		{"linux", "5900", true},
		{"linux", "5909", true},
	} {
		if _, err := parseForwardPort(c.goos, c.arg); (err == nil) != c.ok {
			t.Errorf("parseForwardPort(%s, %s) = %v, want ok=%v", c.goos, c.arg, err, c.ok)
		}
	}
}
