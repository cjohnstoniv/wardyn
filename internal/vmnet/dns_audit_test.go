// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/cjohnstoniv/wardyn/internal/vmnet/vmnettest"
)

func (f *fakeProxy) dnsRecords() []dnsRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dnsRecord(nil), f.dns...)
}

func (f *fakeProxy) dnsWith(want dnsRecord) func() bool {
	return func() bool {
		for _, q := range f.dnsRecords() {
			if q == want {
				return true
			}
		}
		return false
	}
}

// An A query still gets its synthetic address and fills the name table, a TCP
// flow to that address reaches the allowed host through the proxy, and every
// query's real outcome reaches the proxy as a DNS record.
func TestRelayDNSAuditKeepsSyntheticAnswers(t *testing.T) {
	defer func(h, w time.Duration) { helloBudget, firstWait = h, w }(helloBudget, firstWait)
	helloBudget, firstWait = 500*time.Millisecond, 500*time.Millisecond
	f := startFakeProxy(t, "origin.test:443")
	rl, g := newTestRelay(t, f)

	addr := g.Resolve(t, "origin", "test")
	if got, ok := rl.names.lookup(addr); !ok || got != "origin.test" {
		t.Fatalf("name table maps %v to %q, %v; want origin.test", addr, got, ok)
	}
	answered := dnsRecord{Name: "origin.test", Type: "A", Outcome: dnsAnswered, Answer: net.IP(addr[:]).String()}
	waitFor(t, "answered A record", f.dnsWith(answered))

	c, err := g.Dial(addr, 443, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(c, &tls.Config{ServerName: "origin.test", InsecureSkipVerify: true})
	_ = tc.SetDeadline(time.Now().Add(time.Second))
	_ = tc.Handshake()
	c.Close()
	select {
	case b := <-f.tunnel:
		if len(b) == 0 || b[0] != 0x16 {
			t.Fatalf("tunnel got %d bytes, want a ClientHello", len(b))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no tunnel to the allowed host through the synthetic address")
	}

	if m, err := g.Query([]string{"origin", "test"}, dnsmessage.TypeAAAA); err != nil || m.Header.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("AAAA: %+v, %v", m, err)
	}
	waitFor(t, "AAAA record", f.dnsWith(dnsRecord{Name: "origin.test", Type: "AAAA", Outcome: dnsAnswered}))

	if m, err := g.Query([]string{"evil\r\nx", "test"}, dnsmessage.TypeA); err != nil || m.Header.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("bad name: %+v, %v", m, err)
	}
	waitFor(t, "refused record", f.dnsWith(dnsRecord{Type: "A", Outcome: dnsRefused, Cause: "name"}))
}

// A full name table refuses with cause "full", and the record says so.
func TestDNSRecordOfAFullTable(t *testing.T) {
	tbl := newNameTable(time.Now, 1)
	q := func(label string) []byte {
		wire := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, byte(len(label))}
		wire = append(append(wire, label...), 4, 't', 'e', 's', 't', 0, 0, 1, 0, 1)
		return wire
	}
	if _, res, err := answer(tbl, q("one")); err != nil || res.query().Outcome != dnsAnswered {
		t.Fatalf("first name: %+v, %v", res, err)
	}
	_, res, err := answer(tbl, q("two"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.query(), (dnsRecord{Name: "two.test", Type: "A", Outcome: dnsRefused, Cause: "full"}); got != want {
		t.Fatalf("record %+v, want %+v", got, want)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A preparation relay's refuse-all is recorded as prep_refused. It has no run
// proxy, so the record stays in the relay's log.
func TestPrepDNSRecordsPrepRefused(t *testing.T) {
	var logs lockedBuffer
	prep := newPrepRelay(t, &logs)
	if prep.rep.sendDNS != nil {
		t.Fatal("a preparation relay sends DNS records to a proxy it does not have")
	}
	g := vmnettest.Attach(t, prep.Serve)
	if m, err := g.Query([]string{"origin", "test"}, dnsmessage.TypeA); err != nil || m.Header.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("prep query: %+v, %v", m, err)
	}
	waitFor(t, "prep_refused log line", func() bool {
		s := logs.String()
		return strings.Contains(s, `"msg":"dns","name":"origin.test","type":"A","outcome":"prep_refused"`)
	})
}

// A guest asking one name over and over makes one record per interval, the
// rest counted on it; a guest asking a new name each time is bounded overall.
func TestReporterBoundsDNSRecords(t *testing.T) {
	c := &clock{t: time.Unix(1e9, 0)}
	sent := make(chan dnsRecord, 4*maxDNSReports)
	rp := newReporter(c.now, slog.New(slog.DiscardHandler), func(refusal) {})
	rp.sendDNS = func(q dnsRecord) { sent <- q }
	q := dnsRecord{Name: "flood.test", Type: "A", Outcome: dnsAnswered, Answer: "198.18.0.1"}
	for i := 0; i < 50; i++ {
		rp.dns(q)
	}
	if got := recv(t, sent); got.Repeat != 0 || got.Name != "flood.test" {
		t.Fatalf("first record %+v", got)
	}
	c.t = c.t.Add(reportInterval)
	rp.flush()
	if got := recv(t, sent); got.Repeat != 48 {
		t.Fatalf("flushed record %+v, want repeat 48 (50 queries: the first, then one held carrying 48 more)", got)
	}
	expectNone(t, sent)

	c.t = c.t.Add(reportInterval)
	for i := 0; i < 2*maxDNSReports; i++ {
		rp.dns(dnsRecord{Name: fmt.Sprintf("n%d.test", i), Type: "A", Outcome: dnsAnswered})
	}
	for i := 0; i < maxDNSReports; i++ {
		recv(t, sent)
	}
	expectNone(t, sent)
	c.t = c.t.Add(reportInterval)
	rp.flush()
	if n := len(rp.last); n != 0 {
		t.Fatalf("%d DNS keys kept after their interval passed", n)
	}
}

// A guest asking new names, each twice, for 120 seconds keeps at most one
// interval's worth of names held or remembered.
func TestReporterDNSHeldStaysBounded(t *testing.T) {
	c := &clock{t: time.Unix(1e9, 0)}
	rp := newReporter(c.now, slog.New(slog.DiscardHandler), func(refusal) {})
	rp.sendDNS = func(dnsRecord) {}
	n := 0
	for sec := 0; sec < 120; sec++ {
		c.t = c.t.Add(reportInterval) // a new window: new names spend its budget before the flush runs
		for i := 0; i < 2*maxDNSReports; i++ {
			q := dnsRecord{Name: fmt.Sprintf("n%d.test", n), Type: "A", Outcome: dnsAnswered}
			n++
			rp.dns(q)
			rp.dns(q)
		}
		rp.flush()
		rp.mu.Lock()
		held, last := len(rp.held), len(rp.last)
		rp.mu.Unlock()
		if held > maxDNSReports || last > 2*maxDNSReports {
			t.Fatalf("second %d: %d held, %d remembered", sec, held, last)
		}
	}
}

func recv(t *testing.T, ch chan dnsRecord) dnsRecord {
	t.Helper()
	select {
	case q := <-ch:
		return q
	case <-time.After(2 * time.Second):
		t.Fatal("no DNS record sent")
	}
	return dnsRecord{}
}

func expectNone(t *testing.T, ch chan dnsRecord) {
	t.Helper()
	select {
	case q := <-ch:
		t.Fatalf("unexpected DNS record %+v", q)
	case <-time.After(100 * time.Millisecond):
	}
}

// proxyOnlyNet is the DNS audit test's network: a dial that records each
// address it is asked for, and a resolver that counts its dials and refuses
// them, swapped in as net.DefaultResolver for the test.
type proxyOnlyNet struct {
	mu       sync.Mutex
	dialed   []string
	resolved int
	resolver *net.Resolver
}

func newProxyOnlyNet() *proxyOnlyNet {
	n := &proxyOnlyNet{}
	n.resolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		n.mu.Lock()
		n.resolved++
		n.mu.Unlock()
		return nil, fmt.Errorf("no resolver in this test")
	}}
	return n
}

func (n *proxyOnlyNet) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	n.mu.Lock()
	n.dialed = append(n.dialed, addr)
	n.mu.Unlock()
	// The test's own resolver, not net.DefaultResolver: a zero Dialer reads the
	// global on every dial, and a report's dial can outlive the test's swap.
	return (&net.Dialer{Resolver: n.resolver}).DialContext(ctx, network, addr)
}

// ownResolverDial is the production dialer with a resolver of its own. A Dialer with no Resolver reads
// net.DefaultResolver on every dial, and a report dial a test leaves behind would race
// TestRelayDNSAuditDialsOnlyTheProxy's swap of that global.
func ownResolverDial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := proxyDialer()
	d.Resolver = &net.Resolver{}
	return d.DialContext(ctx, network, addr)
}

// Nothing on the VM DNS path dials anywhere but the run's proxy, and nothing
// in the process resolves a name: the relay answers from its table alone.
func TestRelayDNSAuditDialsOnlyTheProxy(t *testing.T) {
	f := startFakeProxy(t)
	pn := newProxyOnlyNet()
	defer func(r *net.Resolver) { net.DefaultResolver = r }(net.DefaultResolver)
	net.DefaultResolver = pn.resolver
	rl, err := New(Config{Proxy: f.addr, Dial: pn.dial})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Close)
	g := vmnettest.Attach(t, rl.Serve)
	for _, n := range []string{"a", "b", "c"} {
		g.Resolve(t, n, "test")
		waitFor(t, n+" record", func() bool {
			for _, q := range f.dnsRecords() {
				if q.Name == n+".test" {
					return true
				}
			}
			return false
		})
	}
	pn.mu.Lock()
	defer pn.mu.Unlock()
	for _, a := range pn.dialed {
		if a != f.addr {
			t.Fatalf("dialed %q; only the proxy %q may be dialed", a, f.addr)
		}
	}
	if pn.resolved != 0 {
		t.Fatalf("%d resolver dials on the VM DNS path", pn.resolved)
	}
}

// A DNS report's post, and a dial its transport starts in the background, can
// outlive the record the test waited for, so the dial must not read
// net.DefaultResolver while the test swaps or restores it.
func TestProxyOnlyNetDialLeavesTheDefaultResolver(t *testing.T) {
	f := startFakeProxy(t)
	pn := newProxyOnlyNet()
	c := newProxyClient(f.addr, pn.dial)
	done := make(chan error, 1)
	go func() { done <- c.sendDNS(dnsRecord{Name: "a.test", Type: "A", Outcome: dnsAnswered}) }()
	r := net.DefaultResolver
	net.DefaultResolver = r // unordered with the report's dial, as the restore is
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
