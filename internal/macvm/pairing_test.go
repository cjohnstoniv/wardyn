// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// pairKey is the base's key in these tests, printable so a leak of it shows in any text it reaches;
// wrongKey is another base's.
var (
	pairKey  = []byte("wardyn-test-pair-key-0123456789!")
	wrongKey = []byte("not-the-key-of-this-base-0123456")
)

// pairVM is a fake VM with a boot's pairer, keyed by pairKey, on port.
func pairVM(t *testing.T, port uint32) (*macvmtest.Machine, func(context.Context) (net.Conn, error), *atomic.Int32) {
	t.Helper()
	nic, err := os.CreateTemp(t.TempDir(), "nic")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nic.Close() })
	vm, err := (&macvmtest.Engine{}).NewMachine(macvm.Plan{
		Network: []macvm.NetworkDevice{{Kind: macvm.NetworkFileHandle, File: nic}},
		Storage: []macvm.StorageDevice{{Path: filepath.Join(t.TempDir(), macvm.DiskFile)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := vm.(*macvmtest.Machine)
	l, err := m.Listen(port)
	if err != nil {
		t.Fatal(err)
	}
	take, connected := macvm.PairOn(t, l, port, pairKey)
	return m, take, connected
}

// dialAgent is the real guest agent's dial: it answers the challenge with key for port.
func dialAgent(t *testing.T, m *macvmtest.Machine, port uint32, key []byte) net.Conn {
	t.Helper()
	c, err := m.DialPaired(port, key)
	if err != nil {
		t.Fatalf("the guest agent's dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// rawDial dials port as any process in the guest can, and answers nothing by itself.
func rawDial(t *testing.T, m *macvmtest.Machine, port uint32) net.Conn {
	t.Helper()
	c, err := m.DialGuest(port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// hostClosed is closed once the host closes c; until then it drains what the host sends.
func hostClosed(c net.Conn) <-chan struct{} {
	ch := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, c); close(ch) }()
	return ch
}

// echo dials port, sends the host's first bytes straight back, and drains the rest.
func echo(t *testing.T, m *macvmtest.Machine, port uint32) <-chan struct{} {
	t.Helper()
	c := rawDial(t, m, port)
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		b := make([]byte, 64)
		n, err := c.Read(b)
		if err != nil {
			return
		}
		_, _ = c.Write(b[:n])
		_, _ = io.Copy(io.Discard, c)
	}()
	return ch
}

func closedWithin(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("the host kept %s open for %s", what, d)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// takesReal checks that take hands out the real agent's connection, and that it still carries bytes.
func takesReal(t *testing.T, take func(context.Context) (net.Conn, error), real net.Conn, within time.Duration) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	h, err := take(ctx)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	go func() { _, _ = h.Write([]byte("ping")) }()
	_ = real.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 4)
	if _, err := io.ReadFull(real, b); err != nil || string(b) != "ping" {
		t.Fatalf("take handed out a connection that is not the real agent's: %q %v", b, err)
	}
	_ = real.SetReadDeadline(time.Time{})
	return h
}

// noGuest checks that take has nothing to hand out for d.
func noGuest(t *testing.T, take func(context.Context) (net.Conn, error), d time.Duration, why string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if c, err := take(ctx); err == nil {
		_ = c.Close()
		t.Fatalf("take handed out a connection: %s", why)
	}
}

// A dialer without the base's key never becomes the idle connection, never closes the real agent's,
// and is no connection for the events.
func TestPairerRefusesADialerWithoutTheKey(t *testing.T) {
	defer macvm.SetPairDeadline(time.Second)()
	m, take, connected := pairVM(t, macvm.AgentPort)
	real := dialAgent(t, m, macvm.AgentPort, pairKey)
	waitFor(t, "the real agent to pair", func() bool { return connected.Load() == 1 })
	imp := dialAgent(t, m, macvm.AgentPort, wrongKey)
	closedWithin(t, hostClosed(imp), 3*time.Second, "a dialer without the key")
	if n := connected.Load(); n != 1 {
		t.Fatalf("connected fired %d times; a dialer without the key is not a guest connection", n)
	}
	takesReal(t, take, real, 2*time.Second)
}

// The real agent pairs, then a process that echoes the challenge dials: the real agent stays paired.
func TestPairerKeepsTheRealAgentWhenAnImpostorDials(t *testing.T) {
	defer macvm.SetPairDeadline(time.Second)()
	m, take, connected := pairVM(t, macvm.AgentPort)
	real := dialAgent(t, m, macvm.AgentPort, pairKey)
	waitFor(t, "the real agent to pair", func() bool { return connected.Load() == 1 })
	closedWithin(t, echo(t, m, macvm.AgentPort), 3*time.Second, "a dialer that echoed the challenge")
	takesReal(t, take, real, 2*time.Second)
	if n := connected.Load(); n != 1 {
		t.Fatalf("connected fired %d times", n)
	}
}

// An impostor that dials first and never answers does not hold the real agent off: the real agent
// pairs while the impostor's proof is still pending.
func TestPairerTakesTheRealAgentAfterAnImpostorDialsFirst(t *testing.T) {
	defer macvm.SetPairDeadline(3 * time.Second)()
	m, take, connected := pairVM(t, macvm.AgentPort)
	silent := hostClosed(rawDial(t, m, macvm.AgentPort))
	start := time.Now()
	real := dialAgent(t, m, macvm.AgentPort, pairKey)
	takesReal(t, take, real, time.Second)
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("the real agent waited %s behind an impostor's pending proof", waited)
	}
	closedWithin(t, silent, 5*time.Second, "an impostor that never answered")
	if n := connected.Load(); n != 1 {
		t.Fatalf("connected fired %d times", n)
	}
}

// The port is inside the proof: an answer made for 1025 is no answer on 1024.
func TestPairerBindsThePortIntoTheProof(t *testing.T) {
	defer macvm.SetPairDeadline(time.Second)()
	m, take, connected := pairVM(t, macvm.AgentPort)
	closedWithin(t, hostClosed(dialOtherPort(t, m)), 3*time.Second, "an answer made for another port")
	noGuest(t, take, 200*time.Millisecond, "an answer for 1025 paired on 1024")
	if n := connected.Load(); n != 0 {
		t.Fatalf("connected fired %d times", n)
	}
}

// dialOtherPort dials 1024 with the base's key and answers as if for 1025.
func dialOtherPort(t *testing.T, m *macvmtest.Machine) net.Conn {
	t.Helper()
	c := rawDial(t, m, macvm.AgentPort)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame := make([]byte, 40)
	if _, err := io.ReadFull(c, frame); err != nil {
		t.Fatalf("no challenge from the host: %v", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	if _, err := c.Write(proof(pairKey, macvm.DesktopPort, frame[8:])); err != nil {
		t.Fatal(err)
	}
	return c
}

// proof is HMAC-SHA256(key, "wardyn-guest-pair-v1" || port (big-endian 32) || challenge), worked here
// from its definition rather than through the code under test.
func proof(key []byte, port uint32, challenge []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("wardyn-guest-pair-v1"))
	mac.Write(binary.BigEndian.AppendUint32(nil, port))
	mac.Write(challenge)
	return mac.Sum(nil)
}

// The host's challenge is "WDYNPAIR" and 32 random bytes; the answer is the proof over the port and that
// challenge, and only then is the connection the idle one.
func TestPairerSendsTheChallengeFrame(t *testing.T) {
	defer macvm.SetPairDeadline(time.Second)()
	m, take, _ := pairVM(t, macvm.AgentPort)
	frames := make([][]byte, 2)
	var first, real net.Conn
	for i := range frames {
		c := rawDial(t, m, macvm.AgentPort)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		frames[i] = make([]byte, 40)
		if _, err := io.ReadFull(c, frames[i]); err != nil {
			t.Fatalf("no 40-byte challenge from the host: %v", err)
		}
		_ = c.SetReadDeadline(time.Time{})
		if string(frames[i][:8]) != "WDYNPAIR" {
			t.Fatalf("challenge %q does not open with the magic", frames[i][:8])
		}
		if _, err := c.Write(proof(pairKey, macvm.AgentPort, frames[i][8:])); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = c
		}
		real = c
	}
	if hmac.Equal(frames[0][8:], frames[1][8:]) {
		t.Fatal("two connections got the same challenge")
	}
	closedWithin(t, hostClosed(first), 2*time.Second, "the older proven connection")
	takesReal(t, take, real, 2*time.Second)
}

// A dialer that reads the challenge and never answers is closed at the deadline, never becomes the
// idle connection, and does not block the accept loop meanwhile.
func TestPairerTimesOutASilentDialer(t *testing.T) {
	const deadline = 2 * time.Second
	defer macvm.SetPairDeadline(deadline)()
	m, take, connected := pairVM(t, macvm.AgentPort)
	start := time.Now()
	silent := hostClosed(rawDial(t, m, macvm.AgentPort))
	noGuest(t, take, 200*time.Millisecond, "a dialer that sent no proof became the idle connection")
	real := dialAgent(t, m, macvm.AgentPort, pairKey)
	takesReal(t, take, real, time.Second)
	if time.Since(start) >= deadline {
		t.Fatalf("the real agent paired only after the silent dialer's deadline (%s)", time.Since(start))
	}
	closedWithin(t, silent, deadline+time.Second, "a dialer that never answered")
	if n := connected.Load(); n != 1 {
		t.Fatalf("connected fired %d times", n)
	}
}

// At most PairPendingMax unproven connections wait on a port; any more are closed at once. The real
// agent, redialling as it does, still pairs once the pending ones time out.
func TestPairerCapsPendingHandshakes(t *testing.T) {
	if macvm.PairPendingMax != 4 {
		t.Fatalf("PairPendingMax = %d, want 4", macvm.PairPendingMax)
	}
	const deadline = 1500 * time.Millisecond
	defer macvm.SetPairDeadline(deadline)()
	m, take, connected := pairVM(t, macvm.AgentPort)
	const extra = 3
	var closed atomic.Int32
	for range macvm.PairPendingMax + extra {
		ch := hostClosed(rawDial(t, m, macvm.AgentPort))
		go func() { <-ch; closed.Add(1) }()
	}
	time.Sleep(300 * time.Millisecond)
	if n := closed.Load(); n != extra {
		t.Fatalf("%d of %d silent dialers were closed at once; want exactly the %d over the cap", n, macvm.PairPendingMax+extra, extra)
	}
	paired := make(chan net.Conn, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			c, err := m.DialPaired(macvm.AgentPort, pairKey)
			if err == nil {
				paired <- c
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	var real net.Conn
	select {
	case real = <-paired:
		t.Cleanup(func() { _ = real.Close() })
	case <-time.After(2*deadline + time.Second):
		t.Fatal("the real agent never paired")
	}
	takesReal(t, take, real, 2*time.Second)
	if n := connected.Load(); n != 1 {
		t.Fatalf("connected fired %d times", n)
	}
}

// A paired connection outlives the proof's deadline: the idle connection carries a session that starts
// long after it paired.
func TestPairerClearsTheProofDeadline(t *testing.T) {
	defer macvm.SetPairDeadline(50 * time.Millisecond)()
	m, take, connected := pairVM(t, macvm.AgentPort)
	real := dialAgent(t, m, macvm.AgentPort, pairKey)
	waitFor(t, "the real agent to pair", func() bool { return connected.Load() == 1 })
	time.Sleep(200 * time.Millisecond)
	h := takesReal(t, take, real, time.Second)
	go func() { _, _ = real.Write([]byte("pong")) }()
	_ = h.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 4)
	if _, err := io.ReadFull(h, b); err != nil || string(b) != "pong" {
		t.Fatalf("the paired connection no longer reads after the proof deadline: %q %v", b, err)
	}
}
