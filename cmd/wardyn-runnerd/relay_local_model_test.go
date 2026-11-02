// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// modelServer is an echo server on 127.0.0.2 standing in for the model server (on another loopback
// address than the relay's 127.0.0.1 bind); accepted counts the connections that reached it. The relay
// later binds 127.0.0.1 at the same port, so a port another process holds there is passed over (a
// parallel package in the docker suite once held it: "bind: address already in use").
func modelServer(t *testing.T) (port uint16, accepted *atomic.Int32) {
	t.Helper()
	var ln net.Listener
	for range 20 {
		l, err := net.Listen("tcp", "127.0.0.2:0")
		if err != nil {
			t.Skipf("no 127.0.0.2 on this host: %v", err)
		}
		probe, err := net.Listen("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(l.Addr().(*net.TCPAddr).Port)).String())
		if err == nil {
			_ = probe.Close()
			ln = l
			break
		}
		_ = l.Close()
	}
	if ln == nil {
		t.Fatal("no port free at both 127.0.0.2 and 127.0.0.1 in 20 tries")
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted = new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return uint16(ln.Addr().(*net.TCPAddr).Port), accepted
}

// modelRelay is a relay whose person allows ports (--local-model-port).
func modelRelay(ports ...uint16) *localRelay {
	return &localRelay{ctx: context.Background(), pair: pair("127.0.0.1", "host-gateway"), listeners: map[uint16]net.Listener{}, conns: map[net.Conn]*runnerwire.Peer{},
		modelAddr: netip.MustParseAddr("127.0.0.2"), modelPorts: ports}
}

// askModel connects from src to the relay at port, sends a request and returns what came back.
func askModel(t *testing.T, src string, port uint16) (string, error) {
	t.Helper()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}, Timeout: 2 * time.Second}
	c, err := d.Dial("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port).String())
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("POST /api/chat")); err != nil {
		return "", nil // closed before the write: nothing relayed
	}
	_ = c.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(c)
	return string(got), nil
}

// A laptop local model's port (#2957) is served at the relay's bind to a flagged run's proxy and
// forwarded to the model server's address at that port alone. The organisation relay's own port is
// refused, and where the bind is the model server's own address nothing is opened.
func TestLocalModelRelayForwardsItsPortToTheModelServer(t *testing.T) {
	port, _ := modelServer(t)
	r := modelRelay(port)
	defer r.Close()
	run := uuid.New()
	for range 2 {
		if err := r.OpenLocalModel(run, []uint16{port}, []netip.Addr{netip.MustParseAddr("127.0.0.1")}); err != nil {
			t.Fatalf("OpenLocalModel: %v", err)
		}
	}
	if len(r.models) != 1 {
		t.Fatalf("listeners %v, want one", r.models)
	}
	if got, err := askModel(t, "127.0.0.1", port); err != nil || got != "POST /api/chat" {
		t.Fatalf("the model server answered %q (%v) through the relay", got, err)
	}

	r.modelPorts = append(r.modelPorts, 443)
	r.listeners[443] = r.models[port].ln // stands in for the organisation relay's listener on 443
	if err := r.OpenLocalModel(uuid.New(), []uint16{443}, []netip.Addr{netip.MustParseAddr("127.0.0.1")}); err == nil || !strings.Contains(err.Error(), "organisation relay's own") {
		t.Errorf("the organisation relay's port = %v", err)
	}
	delete(r.listeners, 443)
	if _, err := r.Ensure(&url.URL{Scheme: "https", Host: "org.example:" + strconv.Itoa(int(port))}); err == nil {
		t.Error("the organisation relay took a local model server's port")
	}

	direct := &localRelay{ctx: context.Background(), pair: pair("127.0.0.1", "host-gateway"), listeners: map[uint16]net.Listener{}, conns: map[net.Conn]*runnerwire.Peer{}, modelPorts: []uint16{11434}}
	if err := direct.OpenLocalModel(uuid.New(), []uint16{11434}, []netip.Addr{netip.MustParseAddr("172.30.0.9")}); err != nil || len(direct.models) != 0 {
		t.Errorf("a loopback bind opened %v (%v): the model server answers there itself", direct.models, err)
	}
}

// Only a flagged run's proxy is relayed: a connection from any other address (another container on
// the bridge, a process on this computer) is closed before a byte reaches the model server.
func TestLocalModelRelayRefusesASourceThatIsNoFlaggedRunsProxy(t *testing.T) {
	port, accepted := modelServer(t)
	r := modelRelay(port)
	defer r.Close()
	if err := r.OpenLocalModel(uuid.New(), []uint16{port}, []netip.Addr{netip.MustParseAddr("127.0.0.3")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := askModel(t, "127.0.0.4", port); got != "" || accepted.Load() != 0 {
		t.Fatalf("a connection from 127.0.0.4, no flagged run's proxy, reached the model server (%d connection(s), answer %q)", accepted.Load(), got)
	}
	if got, err := askModel(t, "127.0.0.3", port); err != nil || got != "POST /api/chat" {
		t.Errorf("the flagged run's proxy at 127.0.0.3 got %q (%v)", got, err)
	}
}

// A port's listener lives while a flagged run uses it: a run that ends takes its proxy's connections
// and admission with it, the last run's end closes the listener, and so does the runner's stop.
func TestLocalModelRelayClosesWhenTheLastFlaggedRunEnds(t *testing.T) {
	port, _ := modelServer(t)
	r := modelRelay(port)
	defer r.Close()
	a, b := uuid.New(), uuid.New()
	for run, peer := range map[uuid.UUID]string{a: "127.0.0.3", b: "127.0.0.5"} {
		if err := r.OpenLocalModel(run, []uint16{port}, []netip.Addr{netip.MustParseAddr(peer)}); err != nil {
			t.Fatal(err)
		}
	}
	r.CloseLocalModel(a)
	if got, _ := askModel(t, "127.0.0.3", port); got != "" {
		t.Errorf("an ended run's proxy at 127.0.0.3 was still relayed (%q)", got)
	}
	if got, err := askModel(t, "127.0.0.5", port); err != nil || got != "POST /api/chat" {
		t.Errorf("a live run's proxy lost the relay when another ended: %q (%v)", got, err)
	}
	r.CloseLocalModel(b)
	if _, err := askModel(t, "127.0.0.5", port); err == nil || len(r.models) != 0 {
		t.Errorf("the listener at 127.0.0.1:%d still accepts after the last flagged run ended (%v)", port, r.models)
	}

	if err := r.OpenLocalModel(a, []uint16{port}, []netip.Addr{netip.MustParseAddr("127.0.0.3")}); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if _, err := askModel(t, "127.0.0.3", port); err == nil {
		t.Errorf("the listener at 127.0.0.1:%d still accepts after the runner stopped", port)
	}
	if err := r.OpenLocalModel(b, []uint16{port}, []netip.Addr{netip.MustParseAddr("127.0.0.5")}); err == nil {
		t.Error("a stopped runner's relay opened a local model port")
	}
}

// The model server is dialled only once an admitted connection sends a first byte within the relay's
// wait: one that sends nothing is closed and never reaches the server.
func TestLocalModelRelayDialsOnlyAfterAFirstByte(t *testing.T) {
	port, accepted := modelServer(t)
	r := modelRelay(port)
	r.firstByte = 50 * time.Millisecond
	defer r.Close()
	if err := r.OpenLocalModel(uuid.New(), []uint16{port}, []netip.Addr{netip.MustParseAddr("127.0.0.3")}); err != nil {
		t.Fatal(err)
	}
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.3")}}
	c, err := d.Dial("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port).String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || accepted.Load() != 0 {
		t.Errorf("a silent connection was kept (%v) or dialled the model server (%d connection(s))", err, accepted.Load())
	}
}

// A port the person has not allowed (--local-model-port), or any port when they allowed none, is
// refused and nothing listens on it, on a native engine and on a VM-backed one alike.
func TestLocalModelRelayRefusesAPortThePersonHasNotAllowed(t *testing.T) {
	port, _ := modelServer(t)
	peers := []netip.Addr{netip.MustParseAddr("127.0.0.3")}
	for name, r := range map[string]*localRelay{"none allowed": modelRelay(), "another port allowed": modelRelay(port + 1)} {
		err := r.OpenLocalModel(uuid.New(), []uint16{port}, peers)
		if err == nil || !strings.Contains(err.Error(), "not a local model port this runner allows") || len(r.models) != 0 {
			t.Errorf("%s: port %d = %v, listeners %v; want it refused", name, port, err, r.models)
		}
		_ = r.Close()
	}
	vm := &localRelay{ctx: context.Background(), pair: pair("127.0.0.1", "host-gateway"), listeners: map[uint16]net.Listener{}, conns: map[net.Conn]*runnerwire.Peer{}}
	if err := vm.OpenLocalModel(uuid.New(), []uint16{11434}, peers); err == nil {
		t.Error("a VM-backed engine's relay admitted a port its person has not allowed")
	}
}
