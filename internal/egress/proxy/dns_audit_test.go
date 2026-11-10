// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func postDNS(t *testing.T, url, body string) int {
	t.Helper()
	resp, err := http.Post(url+routeVMNetDNS, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// dnsRows gives the DNS query rows in buf, and checks each row's frame.
func dnsRows(t *testing.T, buf *syncBuf) []egress.DNSQuery {
	t.Helper()
	var out []egress.DNSQuery
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var d egress.DecisionLog
		if l == "" || json.Unmarshal([]byte(l), &d) != nil || d.DNS == nil {
			continue
		}
		if d.RuleSource != "dns:"+d.DNS.Outcome || d.Request.Host != d.DNS.Name {
			t.Fatalf("DNS row %+v", d)
		}
		out = append(out, *d.DNS)
	}
	return out
}

// TestVMNetDNSRowsFromARealRelay: a guest's lookups through a real relay keep
// their synthetic answer and reach this proxy as DNS rows, beside the flow's
// own allow row.
func TestVMNetDNSRowsFromARealRelay(t *testing.T) {
	g, buf := vmRun(t, types.RunPolicySpec{AllowedDomains: []string{"origin.test"}}, "127.0.0.1:1")
	addr := g.Resolve(t, "origin", "test")
	if _, err := g.Query([]string{"evil\r\nx", "test"}, dnsmessage.TypeA); err != nil {
		t.Fatal(err)
	}
	want := []egress.DNSQuery{
		{Name: "origin.test", Type: "A", Outcome: "answered", Answer: net.IP(addr[:]).String(), Via: "vm_relay"},
		{Type: "A", Outcome: "refused", Cause: "name", Via: "vm_relay"},
	}
	var got []egress.DNSQuery
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if got = dnsRows(t, buf); len(got) >= len(want) {
			break
		}
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("DNS rows %+v, want %+v", got, want)
	}
}

// TestVMNetDNSRows: the relay's answered, refused and prep_refused records each
// become one DNS row with name, type and outcome; a malformed record is refused
// and writes nothing; the route counts only on the relay's lane.
func TestVMNetDNSRows(t *testing.T) {
	p, buf := newRuleTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"origin.test"}}, "127.0.0.1:1", true, "", nil)
	srv := relayServer(p)
	defer srv.Close()
	good := []struct {
		body string
		want egress.DNSQuery
	}{
		{`{"name":"origin.test","type":"A","outcome":"answered","answer":"198.18.0.7"}`,
			egress.DNSQuery{Name: "origin.test", Type: "A", Outcome: "answered", Answer: "198.18.0.7", Via: "vm_relay"}},
		{`{"name":"origin.test","type":"AAAA","outcome":"answered","repeat":3}`,
			egress.DNSQuery{Name: "origin.test", Type: "AAAA", Outcome: "answered", Via: "vm_relay", Repeat: 3}},
		{`{"type":"A","outcome":"refused","cause":"name"}`,
			egress.DNSQuery{Type: "A", Outcome: "refused", Cause: "name", Via: "vm_relay"}},
		{`{"name":"two.test","type":"A","outcome":"refused","cause":"full"}`,
			egress.DNSQuery{Name: "two.test", Type: "A", Outcome: "refused", Cause: "full", Via: "vm_relay"}},
		{`{"name":"setup.test","type":"A","outcome":"prep_refused"}`,
			egress.DNSQuery{Name: "setup.test", Type: "A", Outcome: "prep_refused", Via: "vm_relay"}},
	}
	for _, g := range good {
		if code := postDNS(t, srv.URL, g.body); code != http.StatusNoContent {
			t.Fatalf("%s: %d", g.body, code)
		}
	}
	for _, bad := range []string{
		`{"name":"a\r\nb.test","type":"A","outcome":"answered"}`,
		`{"name":"x.test","type":"A","outcome":"resolved"}`,
		`{"name":"x.test","type":"A","outcome":"refused","cause":"other"}`,
		`{"name":"x.test","type":"A","outcome":"answered","answer":"not-an-ip"}`,
		`{"name":"x.test","type":"A","outcome":"answered","repeat":-1}`,
		`{"name":"x.test","type":"A","outcome":"answered","extra":1}`,
		`{"name":"x.test","outcome":"answered"}`,
	} {
		if code := postDNS(t, srv.URL, bad); code != http.StatusBadRequest {
			t.Fatalf("%s: %d, want 400", bad, code)
		}
	}
	agent := httptest.NewServer(p)
	defer agent.Close()
	if code := postDNS(t, agent.URL, good[0].body); code == http.StatusNoContent {
		t.Fatal("the agent's listener took a relay DNS record")
	}
	got := dnsRows(t, buf)
	if len(got) != len(good) {
		t.Fatalf("%d DNS rows, want %d: %+v", len(got), len(good), got)
	}
	for i, g := range good {
		if got[i] != g.want {
			t.Fatalf("row %d: %+v, want %+v", i, got[i], g.want)
		}
	}
	if rs := rows(t, buf); len(rs) != 0 {
		t.Fatalf("DNS records wrote egress rows: %+v", rs)
	}
}

func dnsQueryWire(t *testing.T, name string, qt dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x4242, RecursionDesired: true})
	_ = b.StartQuestions()
	if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: qt, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type dnsRowLog struct {
	mu   sync.Mutex
	rows []egress.DNSQuery
}

func (l *dnsRowLog) record(q egress.DNSQuery) {
	l.mu.Lock()
	l.rows = append(l.rows, q)
	l.mu.Unlock()
}

func (l *dnsRowLog) get() []egress.DNSQuery {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]egress.DNSQuery(nil), l.rows...)
}

func startDNSListener(t *testing.T, now func() time.Time, rec func(egress.DNSQuery)) (string, string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveDNS(ctx, pc, ln, newDNSLimiter(now, rec)) }()
	t.Cleanup(func() { cancel(); <-done })
	return pc.LocalAddr().String(), ln.Addr().String()
}

// TestDNSListenerRefusesAndRecords: over UDP and TCP the listener answers every
// query REFUSED with no answer and writes its row, and nothing in the process
// dials a resolver.
func TestDNSListenerRefusesAndRecords(t *testing.T) {
	resolverDials := 0
	var mu sync.Mutex
	defer func(r *net.Resolver) { net.DefaultResolver = r }(net.DefaultResolver)
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		mu.Lock()
		resolverDials++
		mu.Unlock()
		return nil, errors.New("no resolver in this test")
	}}
	var log dnsRowLog
	udpAddr, tcpAddr := startDNSListener(t, time.Now, log.record)

	c, err := net.Dial("udp", udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(dnsQueryWire(t, "exfil.example.com.", dnsmessage.TypeA)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	checkRefused(t, buf[:n], "exfil.example.com.")

	tc, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	_ = tc.SetDeadline(time.Now().Add(3 * time.Second))
	q := dnsQueryWire(t, "other.example.com.", dnsmessage.TypeTXT)
	if _, err := tc.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...)); err != nil {
		t.Fatal(err)
	}
	var size [2]byte
	if _, err := io.ReadFull(tc, size[:]); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(tc, resp); err != nil {
		t.Fatal(err)
	}
	checkRefused(t, resp, "other.example.com.")

	want := []egress.DNSQuery{
		{Name: "exfil.example.com", Type: "A", Outcome: "refused", Via: "proxy"},
		{Name: "other.example.com", Type: "TXT", Outcome: "refused", Via: "proxy"},
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(log.get()) < len(want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := log.get()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("rows %+v, want %+v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if resolverDials != 0 {
		t.Fatalf("%d resolver dials from the listener", resolverDials)
	}
}

func checkRefused(t *testing.T, wire []byte, name string) {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(wire); err != nil {
		t.Fatal(err)
	}
	if !m.Header.Response || m.Header.ID != 0x4242 || m.Header.RCode != dnsmessage.RCodeRefused || len(m.Answers) != 0 ||
		len(m.Questions) != 1 || m.Questions[0].Name.String() != name {
		t.Fatalf("reply %+v, want REFUSED for %s with no answer", m, name)
	}
}

// TestDNSListenerRowsAreBounded: one name asked over and over writes one row
// per interval carrying the count; new names are bounded per interval; idle
// names are forgotten.
func TestDNSListenerRowsAreBounded(t *testing.T) {
	now := time.Unix(1e9, 0)
	var log dnsRowLog
	l := newDNSLimiter(func() time.Time { return now }, log.record)
	q := egress.DNSQuery{Name: "flood.test", Type: "A", Outcome: "refused", Via: "proxy"}
	for i := 0; i < 50; i++ {
		l.note(q)
	}
	now = now.Add(dnsInterval)
	l.flush()
	got := log.get()
	if len(got) != 2 || got[0].Repeat != 0 || got[1].Repeat != 48 {
		t.Fatalf("rows %+v, want the first then one carrying 48 more", got)
	}
	now = now.Add(dnsInterval)
	for i := 0; i < 2*maxDNSRows; i++ {
		l.note(egress.DNSQuery{Name: "n" + string(rune('a'+i%26)) + strings.Repeat("x", i/26) + ".test", Type: "A", Outcome: "refused", Via: "proxy"})
	}
	if n := len(log.get()) - 2; n != maxDNSRows {
		t.Fatalf("%d rows for %d new names in one interval, want %d", n, 2*maxDNSRows, maxDNSRows)
	}
	now = now.Add(dnsInterval)
	l.flush()
	if len(l.last) != 0 || len(l.held) != 0 {
		t.Fatalf("kept %d names, %d held after their interval", len(l.last), len(l.held))
	}
}

// TestTakesDNS: rows go to the control plane's endpoint, and to a runner's
// spool only when its accept said it replays them.
func TestTakesDNS(t *testing.T) {
	if (*decisionSink)(nil).takesDNS() {
		t.Fatal("nil sink takes DNS rows")
	}
	if !(&decisionSink{}).takesDNS() {
		t.Fatal("the control plane's endpoint does not take DNS rows")
	}
	r := &remoteSink{}
	if (&decisionSink{remote: r}).takesDNS() {
		t.Fatal("a runner that did not accept DNS rows takes them")
	}
	r.dns.Store(true)
	if !(&decisionSink{remote: r}).takesDNS() {
		t.Fatal("a runner that accepted DNS rows does not take them")
	}
}
