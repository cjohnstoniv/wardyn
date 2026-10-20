// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestValidName(t *testing.T) {
	long := strings.Repeat("a", 63)
	for name, want := range map[string]bool{
		"example.com":                     true,
		"a-b.c0":                          true,
		long + ".com":                     true,
		strings.Repeat("a", 64) + ".com":  false,
		strings.Repeat(long+".", 4) + "x": false, // 257 bytes
		"":                                false,
		"a..b":                            false,
		"-a.com":                          false,
		"a-.com":                          false,
		"a b.com":                         false,
		"evil.com\r\nx-header: 1":         false,
		"evil.com\n":                      false,
		"host:22":                         false,
		"under_score.com":                 false,
		"a\x00.com":                       false,
	} {
		if got := validName(name); got != want {
			t.Errorf("validName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestNameTableDeterministicAcrossRelays(t *testing.T) {
	c := &clock{t: time.Unix(1e9, 0)}
	a, b := newNameTable(c.now, 16), newNameTable(c.now, 16)
	// Different insertion order: a fresh relay (a restored VM's) answers the
	// same name with the same address.
	for _, n := range []string{"one.test", "two.test", "three.test"} {
		_, _, _ = a.resolve(n)
	}
	for _, n := range []string{"three.test", "one.test", "two.test"} {
		_, _, _ = b.resolve(n)
	}
	for _, n := range []string{"one.test", "two.test", "three.test"} {
		x, _, _ := a.resolve(n)
		y, _, _ := b.resolve(n)
		if x != y {
			t.Fatalf("%s: %v in one relay, %v in another", n, x, y)
		}
		if x[0] != 198 || (x[1] != 18 && x[1] != 19) {
			t.Fatalf("%s: %v is outside 198.18.0.0/15", n, x)
		}
		if got, ok := a.lookup(x); !ok || got != n {
			t.Fatalf("lookup(%v) = %q, %v", x, got, ok)
		}
	}
}

// TestNameTableNoAliasing is the regression for the spike's arithmetic: its
// counter ORed past the /15 and relabelled the first name's address after
// 131,070 names. Here every live name keeps a distinct address, collisions
// included, and none is the /15's network or broadcast address.
func TestNameTableNoAliasing(t *testing.T) {
	c := &clock{t: time.Unix(1e9, 0)}
	tb := newNameTable(c.now, maxNames)
	seen := map[[4]byte]string{}
	for i := 0; i < maxNames; i++ {
		n := fmt.Sprintf("n%d.test", i)
		a, _, ok := tb.resolve(n)
		if !ok {
			t.Fatalf("%s refused below the cap", n)
		}
		if prev, dup := seen[a]; dup {
			t.Fatalf("%s and %s share %v", prev, n, a)
		}
		if a == [4]byte{198, 18, 0, 0} || a == [4]byte{198, 19, 255, 255} {
			t.Fatalf("%s got the /15's network or broadcast address", n)
		}
		seen[a] = n
	}
	// Forced collision: occupy a name's home offset with another entry.
	tb2 := newNameTable(c.now, 4)
	home := toAddr(synthBase + derive("victim.test"))
	tb2.byAddr[synthBase+derive("victim.test")] = tb2.lru.PushBack(&nameEntry{name: "squatter.test", addr: synthBase + derive("victim.test"), used: c.t})
	a, _, _ := tb2.resolve("victim.test")
	if a == home {
		t.Fatalf("victim.test took an address another name holds")
	}
}

func TestNameTableCapEvictsOnlyExpired(t *testing.T) {
	c := &clock{t: time.Unix(1e9, 0)}
	tb := newNameTable(c.now, 2)
	_, _, _ = tb.resolve("a.test")
	c.t = c.t.Add(time.Second)
	_, _, _ = tb.resolve("b.test")
	// Full of names the guest may still cache: refused, nothing relabelled.
	if _, _, ok := tb.resolve("c.test"); ok {
		t.Fatal("a table full of live names accepted a new one")
	}
	if _, ok := tb.lookup(toAddr(synthBase + derive("a.test"))); !ok {
		t.Fatal("a refused addition evicted a live name")
	}
	// a.test was just used by that lookup; b.test is the least recent once the TTL passes.
	c.t = c.t.Add(answerTTL + time.Second)
	_, _, _ = tb.resolve("a.test")
	_, evicted, ok := tb.resolve("c.test")
	if !ok || evicted != "b.test" {
		t.Fatalf("resolve c.test = evicted %q ok %v, want b.test evicted", evicted, ok)
	}
	if _, ok := tb.lookup(toAddr(synthBase + derive("b.test"))); ok {
		t.Fatal("an evicted name still maps")
	}
}

func query(t *testing.T, name string, qt dnsmessage.Type) []byte {
	t.Helper()
	n, err := dnsmessage.NewName(name)
	if err != nil {
		t.Fatal(err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: n, Type: qt, Class: dnsmessage.ClassINET})
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAnswer(t *testing.T) {
	c := &clock{t: time.Unix(1e9, 0)}
	tb := newNameTable(c.now, 8)
	parse := func(b []byte) dnsmessage.Message {
		var m dnsmessage.Message
		if err := m.Unpack(b); err != nil {
			t.Fatal(err)
		}
		return m
	}
	resp, res, err := answer(tb, query(t, "Example.COM.", dnsmessage.TypeA))
	if err != nil || !res.given || res.name != "example.com" {
		t.Fatalf("A: %+v %v", res, err)
	}
	m := parse(resp)
	if m.Header.ID != 7 || m.Header.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 1 || m.Answers[0].Header.TTL != 60 {
		t.Fatalf("A reply %+v", m)
	}
	for _, qt := range []dnsmessage.Type{dnsmessage.TypeAAAA, dnsmessage.TypeHTTPS, dnsmessage.TypeTXT, dnsmessage.TypeMX} {
		resp, _, _ := answer(tb, query(t, "example.com.", qt))
		if m := parse(resp); m.Header.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
			t.Fatalf("%v: want NOERROR with no answer, got %v with %d", qt, m.Header.RCode, len(m.Answers))
		}
	}
	// A label carrying CR/LF never reaches the table.
	raw := []byte{0, 9, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 6, 'e', 'v', 'i', 'l', '\r', '\n', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	resp, res, err = answer(tb, raw)
	if err != nil || res.refused != "name" || res.name != "" {
		t.Fatalf("CR/LF label: %+v %v", res, err)
	}
	if m := parse(resp); m.Header.RCode != dnsmessage.RCodeRefused || len(m.Answers) != 0 {
		t.Fatalf("CR/LF label: rcode %v, %d answers", m.Header.RCode, len(m.Answers))
	}
	if len(tb.byName) != 1 {
		t.Fatalf("table holds %d names, want only example.com", len(tb.byName))
	}
	if _, _, err := answer(tb, []byte{1, 2, 3}); err == nil {
		t.Fatal("garbage parsed")
	}
}
