// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
)

// preamble is the bytes a keyed proxy sends first: the magic, version v and key.
func preamble(v byte, key []byte) []byte {
	return append([]byte{relayPreambleMagic, v}, key...)
}

// TestRelayAdmits: one row per case of the k8s relay's rule. R is the run the source address maps
// to, F another run; each row is fenced run × fenced IP × how the connection opened × keys held.
func TestRelayAdmits(t *testing.T) {
	r, f := uuid.New(), uuid.New()
	rKey, fKey := bytes.Repeat([]byte{1}, relayPreambleKeyLen), bytes.Repeat([]byte{2}, relayPreambleKeyLen)
	keyed := func(v byte, key []byte) relayStart {
		s := relayStart{keyed: true, version: v, first: 0x16}
		copy(s.key[:], key)
		return s
	}
	tls := relayStart{first: 0x16}
	both := map[uuid.UUID][sha256.Size]byte{r: sha256.Sum256(rKey), f: sha256.Sum256(fKey)}
	onlyF := map[uuid.UUID][sha256.Size]byte{f: sha256.Sum256(fKey)}
	for _, c := range []struct {
		name      string
		s         relayStart
		keys      map[uuid.UUID][sha256.Size]byte
		runFenced bool
		ipFenced  bool
		admitted  bool
	}{
		{name: "TLS, unfenced, no key held", s: tls, keys: onlyF, admitted: true},
		{name: "TLS, fenced IP", s: tls, keys: onlyF, ipFenced: true},
		{name: "TLS, fenced run", s: tls, keys: onlyF, runFenced: true},
		{name: "TLS, run holds a key", s: tls, keys: both},
		{name: "plain HTTP byte, unfenced, no key held", s: relayStart{first: 'G'}, keys: onlyF, admitted: true},
		{name: "keyed, own key, unfenced", s: keyed(relayPreambleVersion, rKey), keys: both, admitted: true},
		{name: "keyed, own key, fenced IP", s: keyed(relayPreambleVersion, rKey), keys: both, ipFenced: true, admitted: true},
		{name: "keyed, own key, fenced run", s: keyed(relayPreambleVersion, rKey), keys: both, runFenced: true},
		{name: "keyed, another run's key, fenced IP", s: keyed(relayPreambleVersion, fKey), keys: both, ipFenced: true},
		{name: "keyed, another run's key, unfenced", s: keyed(relayPreambleVersion, fKey), keys: both},
		{name: "keyed, run holds no key", s: keyed(relayPreambleVersion, fKey), keys: onlyF},
		{name: "keyed, bad version, own key", s: keyed(relayPreambleVersion+1, rKey), keys: both},
		{name: "keyed, bad version, run holds no key", s: keyed(0, rKey), keys: onlyF},
		{name: "keyed, zero key, run holds no key", s: keyed(relayPreambleVersion, make([]byte, relayPreambleKeyLen)), keys: onlyF},
	} {
		if got := relayAdmits(r, c.s, c.keys, c.runFenced, c.ipFenced); got != c.admitted {
			t.Errorf("%s: admitted %v, want %v", c.name, got, c.admitted)
		}
	}
}

// TestReadRelayStart: a TLS opening is read as its first byte alone; a preamble yields its version,
// key and the stream's own first byte after it; anything short of that is not admitted.
func TestReadRelayStart(t *testing.T) {
	key := bytes.Repeat([]byte{7}, relayPreambleKeyLen)
	for _, c := range []struct {
		name  string
		send  []byte
		ok    bool
		keyed bool
		first byte
	}{
		{name: "TLS", send: []byte{0x16, 3, 1}, ok: true, first: 0x16},
		{name: "preamble then TLS", send: append(preamble(relayPreambleVersion, key), 0x16, 3), ok: true, keyed: true, first: 0x16},
		{name: "preamble, nothing after", send: preamble(relayPreambleVersion, key)},
		{name: "short key", send: preamble(relayPreambleVersion, key[:10])},
		{name: "magic alone", send: []byte{relayPreambleMagic}},
		{name: "nothing", send: nil},
	} {
		a, b := net.Pipe()
		go func() {
			_, _ = b.Write(c.send)
			_ = b.Close()
		}()
		s, ok := readRelayStart(a, time.Now().Add(2*time.Second))
		_ = a.Close()
		if ok != c.ok || ok && (s.keyed != c.keyed || s.first != c.first) {
			t.Errorf("%s: %+v %v", c.name, s, ok)
			continue
		}
		if c.keyed && (s.version != relayPreambleVersion || !bytes.Equal(s.key[:], key)) {
			t.Errorf("%s: version %d key %x", c.name, s.version, s.key)
		}
	}
	// A preamble that stalls is cut at the deadline.
	a, b := net.Pipe()
	defer b.Close()
	go func() { _, _ = b.Write(preamble(relayPreambleVersion, key[:5])) }()
	start := time.Now()
	if _, ok := readRelayStart(a, time.Now().Add(100*time.Millisecond)); ok || time.Since(start) > 2*time.Second {
		t.Fatalf("a stalled preamble: ok %v after %v", ok, time.Since(start))
	}
	_ = a.Close()
}
