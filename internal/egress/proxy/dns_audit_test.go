// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
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
	// The relay sends each record on its own goroutine, so rows arrive in either order.
	if len(got) != len(want) || !slices.Contains(got, want[0]) || !slices.Contains(got, want[1]) {
		t.Fatalf("DNS rows %+v, want %+v in any order", got, want)
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
		{`{"name":"10.1.2.3","type":"A","outcome":"answered","answer":"198.18.0.9"}`,
			egress.DNSQuery{Name: "10.1.2.3", Type: "A", Outcome: "answered", Answer: "198.18.0.9", Via: "vm_relay"}},
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
	// Each listener notes its row after its own reply, the UDP and the TCP one on goroutines of their own, so the
	// two rows may land either way round.
	got := log.get()
	inOrder := len(got) == 2 && got[0] == want[0] && got[1] == want[1]
	swapped := len(got) == 2 && got[0] == want[1] && got[1] == want[0]
	if !inOrder && !swapped {
		t.Fatalf("rows %+v, want %+v in any order", got, want)
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

// TestAnswerDNSOwnName pins which questions get the empty answer: AAAA for the
// proxy's exact name, and nothing else. Mutations: matching with strings.Contains
// or HasPrefix/HasSuffix instead of equality turns the "wardyn-proxy.evil.com."
// / "xwardyn-proxy." / "evil.wardyn-proxy." rows into NOERROR with no record;
// dropping the type check turns the A, TXT and MX rows into NOERROR with no
// record; dropping ToLower fails the upper-case rows; dropping the trailing-dot
// trim fails every own-name row; answering REFUSED for the own name, or
// recording it, fails the own-name rows; setting Authoritative on every reply, or
// on none, fails the AA check (the bit is on the own-name reply only).
func TestAnswerDNSOwnName(t *testing.T) {
	cases := []struct {
		name string
		typ  dnsmessage.Type
		own  bool
	}{
		{"wardyn-proxy.", dnsmessage.TypeAAAA, true},
		{"WARDYN-PROXY.", dnsmessage.TypeAAAA, true},
		{"Wardyn-Proxy.", dnsmessage.TypeAAAA, true},
		{"wardyn-proxy.", dnsmessage.TypeA, false},
		{"WARDYN-PROXY.", dnsmessage.TypeA, false},
		{"wardyn-proxy.", dnsmessage.TypeTXT, false},
		{"wardyn-proxy.", dnsmessage.TypeMX, false},
		{"wardyn-proxy.", dnsmessage.Type(65), false}, // HTTPS
		{"wardyn-proxy.", dnsmessage.TypeALL, false},
		{"example.com.", dnsmessage.TypeAAAA, false},
		{"example.com.", dnsmessage.TypeA, false},
		{"wardyn-proxy.evil.com.", dnsmessage.TypeAAAA, false},
		{"wardyn-proxy.evil.com.", dnsmessage.TypeA, false},
		{"wardyn-proxy.cluster.local.", dnsmessage.TypeAAAA, false},
		{"xwardyn-proxy.", dnsmessage.TypeAAAA, false},
		{"wardyn-proxyx.", dnsmessage.TypeAAAA, false},
		{"evil.wardyn-proxy.", dnsmessage.TypeAAAA, false},
		{"wardyn-proxy.com.", dnsmessage.TypeAAAA, false},
	}
	for _, c := range cases {
		out, q, record, ok := answerDNS(dnsQueryWire(t, c.name, c.typ))
		if !ok {
			t.Fatalf("%s %v: no reply", c.name, c.typ)
		}
		var m dnsmessage.Message
		if err := m.Unpack(out); err != nil {
			t.Fatal(err)
		}
		wantRCode := dnsmessage.RCodeRefused
		if c.own {
			wantRCode = dnsmessage.RCodeSuccess
		}
		if !m.Header.Response || m.Header.ID != 0x4242 || m.Header.RCode != wantRCode || len(m.Answers) != 0 ||
			len(m.Questions) != 1 || m.Questions[0].Name.String() != c.name || m.Questions[0].Type != c.typ {
			t.Errorf("%s %v: reply %+v, want rcode %v, no answers, question echoed", c.name, c.typ, m, wantRCode)
		}
		if m.Header.Authoritative != c.own {
			t.Errorf("%s %v: AA=%v, want %v (set on the own-name empty answer only, so a resolver takes NODATA, not a lame referral)", c.name, c.typ, m.Header.Authoritative, c.own)
		}
		if record == c.own {
			t.Errorf("%s %v: record=%v, want %v", c.name, c.typ, record, !c.own)
		}
		if !c.own && (q.Outcome != "refused" || q.Name != dnsName(c.name)) {
			t.Errorf("%s %v: row %+v, want a refused row for the name", c.name, c.typ, q)
		}
	}
}

// TestAnswerDNSOwnNameNeedsStandardQueryInClass: the empty answer is for a standard
// QUERY (opcode 0) in the Internet class only. IQUERY, STATUS, NOTIFY and UPDATE
// carrying the same question, and a CH or ANY class AAAA, are REFUSED and recorded.
// Mutations: dropping the `h.OpCode == 0` test turns the opcode rows into NOERROR with
// no record; dropping the ClassINET test does the same to the class rows.
func TestAnswerDNSOwnNameNeedsStandardQueryInClass(t *testing.T) {
	wire := func(op dnsmessage.OpCode, class dnsmessage.Class) []byte {
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x4242, OpCode: op, RecursionDesired: true})
		_ = b.StartQuestions()
		if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName("wardyn-proxy."), Type: dnsmessage.TypeAAAA, Class: class}); err != nil {
			t.Fatal(err)
		}
		out, err := b.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	cases := []struct {
		what  string
		op    dnsmessage.OpCode
		class dnsmessage.Class
	}{
		{"IQUERY", 1, dnsmessage.ClassINET},
		{"STATUS", 2, dnsmessage.ClassINET},
		{"NOTIFY", 4, dnsmessage.ClassINET},
		{"UPDATE", 5, dnsmessage.ClassINET},
		{"class CH", 0, dnsmessage.ClassCHAOS},
		{"class ANY", 0, dnsmessage.ClassANY},
	}
	for _, c := range cases {
		out, q, record, ok := answerDNS(wire(c.op, c.class))
		if !ok || !record || q.Outcome != "refused" || q.Name != "wardyn-proxy" || q.Type != "AAAA" {
			t.Errorf("%s: ok=%v record=%v row=%+v, want a refused AAAA row for wardyn-proxy", c.what, ok, record, q)
			continue
		}
		var m dnsmessage.Message
		if err := m.Unpack(out); err != nil {
			t.Fatal(err)
		}
		if m.Header.RCode != dnsmessage.RCodeRefused || m.Header.Authoritative || len(m.Answers) != 0 {
			t.Errorf("%s: reply %+v, want REFUSED, AA=0, no answer", c.what, m.Header)
		}
	}
}

// TestDNSListenerOwnNameWritesNoRow: over UDP and TCP the proxy's own name is
// answered NOERROR with no answer and leaves no row, while the names around it
// are still REFUSED and recorded. The other names are asked AFTER the own-name
// queries and the rows are read once the last of them shows, so a row the own
// name wrongly wrote would be in the log by then. Mutation: calling lim.note
// without the record check on either path puts a wardyn-proxy row in the log;
// treating the own name as refused fails the reply checks.
func TestDNSListenerOwnNameWritesNoRow(t *testing.T) {
	var log dnsRowLog
	udpAddr, tcpAddr := startDNSListener(t, time.Now, log.record)
	ask := func(c net.Conn, name string, qt dnsmessage.Type) []byte {
		q := dnsQueryWire(t, name, qt)
		if _, isTCP := c.(*net.TCPConn); isTCP {
			q = append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...)
		}
		if _, err := c.Write(q); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1500)
		if _, isTCP := c.(*net.TCPConn); isTCP {
			var size [2]byte
			if _, err := io.ReadFull(c, size[:]); err != nil {
				t.Fatal(err)
			}
			buf = make([]byte, binary.BigEndian.Uint16(size[:]))
			if _, err := io.ReadFull(c, buf); err != nil {
				t.Fatal(err)
			}
			return buf
		}
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		return buf[:n]
	}
	empty := func(wire []byte, name string) {
		t.Helper()
		var m dnsmessage.Message
		if err := m.Unpack(wire); err != nil {
			t.Fatal(err)
		}
		if !m.Header.Response || m.Header.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 ||
			len(m.Questions) != 1 || m.Questions[0].Name.String() != name {
			t.Fatalf("reply %+v, want NOERROR with no answer for %s", m, name)
		}
	}

	uc, err := net.Dial("udp", udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	tc, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	_ = uc.SetDeadline(time.Now().Add(3 * time.Second))
	_ = tc.SetDeadline(time.Now().Add(3 * time.Second))

	empty(ask(uc, "wardyn-proxy.", dnsmessage.TypeAAAA), "wardyn-proxy.")
	empty(ask(uc, "WARDYN-PROXY.", dnsmessage.TypeAAAA), "WARDYN-PROXY.")
	empty(ask(tc, "wardyn-proxy.", dnsmessage.TypeAAAA), "wardyn-proxy.")
	checkRefused(t, ask(uc, "example.com.", dnsmessage.TypeAAAA), "example.com.")
	checkRefused(t, ask(tc, "wardyn-proxy.evil.com.", dnsmessage.TypeAAAA), "wardyn-proxy.evil.com.")
	checkRefused(t, ask(uc, "xwardyn-proxy.", dnsmessage.TypeAAAA), "xwardyn-proxy.")
	checkRefused(t, ask(uc, "wardyn-proxy.", dnsmessage.TypeA), "wardyn-proxy.")
	checkRefused(t, ask(tc, "wardyn-proxy.", dnsmessage.TypeMX), "wardyn-proxy.")

	want := map[string]bool{
		"example.com/AAAA": true, "wardyn-proxy.evil.com/AAAA": true, "xwardyn-proxy/AAAA": true,
		"wardyn-proxy/A": true, "wardyn-proxy/MX": true,
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(log.get()) < len(want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // room for a wrongly written row to land
	got := log.get()
	if len(got) != len(want) {
		t.Fatalf("rows %+v, want exactly %v", got, want)
	}
	for _, r := range got {
		if !want[r.Name+"/"+r.Type] || r.Outcome != "refused" || r.Via != "proxy" {
			t.Fatalf("row %+v, want a refused row for one of %v", r, want)
		}
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

// TestDNSListenerHeldStaysBounded: a sandbox asking new names, each twice, for
// 120 seconds keeps at most one interval's worth of names held or remembered.
func TestDNSListenerHeldStaysBounded(t *testing.T) {
	now := time.Unix(1e9, 0)
	l := newDNSLimiter(func() time.Time { return now }, func(egress.DNSQuery) {})
	n := 0
	for sec := 0; sec < 120; sec++ {
		now = now.Add(dnsInterval) // a new window: new names spend its budget before the flush runs
		for i := 0; i < 2*maxDNSRows; i++ {
			q := egress.DNSQuery{Name: fmt.Sprintf("n%d.test", n), Type: "A", Outcome: "refused", Via: "proxy"}
			n++
			l.note(q)
			l.note(q)
		}
		l.flush()
		if len(l.held) > maxDNSRows || len(l.last) > 2*maxDNSRows {
			t.Fatalf("second %d: %d held, %d remembered", sec, len(l.held), len(l.last))
		}
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
