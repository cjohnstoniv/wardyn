// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// Each DNS query a run's sandbox asks becomes one run.dns.query row, written
// from one of two places and never by looking the name up:
//   - a VM guest's relay answers from its own name table and reports each
//     query's outcome here (routeVMNetDNS), on the relay's lane only;
//   - a container or pod agent's resolver is this proxy's listener (ServeDNS),
//     which answers every query REFUSED except an AAAA for its own name, which
//     gets an empty answer and no row: those sandboxes had no DNS before it.
const (
	routeVMNetDNS = "/wardyn/v1/vmnet/dns"

	// DNSListenEnv is the proxy's environment variable naming its DNS listen
	// address (":53"); unset, the proxy has no DNS listener.
	DNSListenEnv = "WARDYN_PROXY_DNS_LISTEN"

	dnsViaVMRelay = "vm_relay"
	dnsViaProxy   = "proxy"

	dnsAnswered    = "answered"
	dnsRefused     = "refused"
	dnsPrepRefused = "prep_refused"

	// dnsInterval and maxDNSRows bound the listener's rows: the first of each
	// (name, type) per interval is written and the rest counted on the next,
	// and at most maxDNSRows go out per interval across all names. The relay
	// bounds its own reports the same way (internal/vmnet/report.go).
	dnsInterval = time.Second
	maxDNSRows  = 64

	maxDNSTCPConns = 16
	dnsTCPIdle     = 5 * time.Second
)

// vmnetDNS is the relay's DNS record (internal/vmnet/report.go dnsRecord).
type vmnetDNS struct {
	Name    string `json:"name,omitempty"`
	Type    string `json:"type"`
	Outcome string `json:"outcome"`
	Cause   string `json:"cause,omitempty"`
	Answer  string `json:"answer,omitempty"`
	Repeat  int    `json:"repeat,omitempty"`
}

// routeVMNetPrefix holds the relay's report routes, which count only on a VM
// run's relay lane; anywhere else they are not found.
const routeVMNetPrefix = "/wardyn/v1/vmnet/"

// handleVMNetReport dispatches the relay's reports: a refusal or a DNS record.
func (p *Proxy) handleVMNetReport(w http.ResponseWriter, r *http.Request) {
	switch {
	case !p.relayLane(r.Context()) || r.Method != http.MethodPost:
		http.Error(w, "unknown brokered route", http.StatusNotFound)
	case r.URL.Path == routeVMNetRefusals:
		p.handleVMNetRefusal(w, r)
	case r.URL.Path == routeVMNetDNS:
		p.handleVMNetDNS(w, r)
	default:
		http.Error(w, "unknown brokered route", http.StatusNotFound)
	}
}

// handleVMNetDNS takes one relay DNS record and writes its row. Every field is
// checked: the relay is the guest's peer, and the guest chose the name.
func (p *Proxy) handleVMNetDNS(w http.ResponseWriter, r *http.Request) {
	var in vmnetDNS
	dec := json.NewDecoder(io.LimitReader(r.Body, maxVMNetRefusalBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		http.Error(w, "bad vmnet dns record", http.StatusBadRequest)
		return
	}
	q, ok := vmnetDNSQuery(in)
	if !ok {
		http.Error(w, "bad vmnet dns record", http.StatusBadRequest)
		return
	}
	p.recordDNS(q)
	w.WriteHeader(http.StatusNoContent)
}

func vmnetDNSQuery(in vmnetDNS) (egress.DNSQuery, bool) {
	q := egress.DNSQuery{Name: dnsName(in.Name), Type: dnsType(in.Type), Outcome: in.Outcome, Cause: in.Cause, Via: dnsViaVMRelay, Repeat: in.Repeat}
	if (in.Name != "" && q.Name == "") || q.Type == "" || in.Repeat < 0 {
		return q, false
	}
	switch {
	case in.Outcome == dnsAnswered && in.Cause == "":
		if in.Answer != "" {
			ip := net.ParseIP(in.Answer).To4()
			if ip == nil {
				return q, false
			}
			q.Answer = ip.String()
		}
	case in.Outcome == dnsRefused && (in.Cause == "name" || in.Cause == "full") && in.Answer == "":
	case in.Outcome == dnsPrepRefused && in.Cause == "" && in.Answer == "":
	default:
		return q, false
	}
	return q, true
}

// dnsName keeps a lower-case LDH name of at most 253 bytes (a dotted quad
// included: a sandbox may ask for one), or gives "".
func dnsName(s string) string {
	return vmnetName(strings.TrimSuffix(strings.ToLower(s), "."))
}

// dnsType keeps a record type's mnemonic ("A", "AAAA") or number.
func dnsType(s string) string {
	s = strings.TrimPrefix(s, "Type")
	if s == "" || len(s) > 10 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return ""
		}
	}
	return s
}

// recordDNS writes one run.dns.query row through the decision sink, when its
// reader takes DNS rows (decisionSink.takesDNS).
func (p *Proxy) recordDNS(q egress.DNSQuery) {
	if !p.sink.takesDNS() {
		return
	}
	d, src := egress.Deny, "dns:"+q.Outcome
	if q.Outcome == dnsAnswered {
		d = egress.Allow
	}
	log := decisionLog(egress.Request{RunID: p.runID, Host: q.Name, Time: p.now()}, d, src)
	log.DNS = &q
	p.sink.emit(log)
}

// dnsLimiter bounds the listener's rows (dnsInterval, maxDNSRows).
type dnsLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	last      map[string]time.Time
	held      map[string]*heldDNS
	from      time.Time
	sent      int
	record    func(egress.DNSQuery)
	overBound int
}

type heldDNS struct {
	q     egress.DNSQuery
	count int
}

func newDNSLimiter(now func() time.Time, record func(egress.DNSQuery)) *dnsLimiter {
	return &dnsLimiter{now: now, record: record, last: map[string]time.Time{}, held: map[string]*heldDNS{}}
}

func (l *dnsLimiter) take(now time.Time) bool {
	if now.Sub(l.from) >= dnsInterval {
		l.from, l.sent = now, 0
	}
	if l.sent >= maxDNSRows {
		l.overBound++
		return false
	}
	l.sent++
	return true
}

func (l *dnsLimiter) note(q egress.DNSQuery) {
	k := q.Name + "\x00" + q.Type
	l.mu.Lock()
	now := l.now()
	if now.Sub(l.last[k]) < dnsInterval {
		h := l.held[k]
		if h == nil {
			h = &heldDNS{}
			l.held[k] = h
		}
		h.q, h.count = q, h.count+1
		l.mu.Unlock()
		return
	}
	if !l.take(now) {
		l.mu.Unlock()
		return
	}
	l.last[k] = now
	if h := l.held[k]; h != nil {
		q.Repeat = h.count
		delete(l.held, k)
	}
	l.mu.Unlock()
	l.record(q)
}

// flush writes every held count whose interval has passed and forgets idle
// names, so a sandbox that asks a new name each time grows nothing.
func (l *dnsLimiter) flush() {
	l.mu.Lock()
	now := l.now()
	var out []egress.DNSQuery
	for k, h := range l.held {
		if now.Sub(l.last[k]) < dnsInterval {
			continue
		}
		if !l.take(now) {
			delete(l.held, k) // over the bound: dropped and counted once, so held stays bounded
			continue
		}
		q := h.q
		q.Repeat = h.count - 1
		out = append(out, q)
		l.last[k] = now
		delete(l.held, k)
	}
	for k, t := range l.last {
		if now.Sub(t) >= dnsInterval && l.held[k] == nil {
			delete(l.last, k)
		}
	}
	over := l.overBound
	l.overBound = 0
	l.mu.Unlock()
	for _, q := range out {
		l.record(q)
	}
	if over > 0 {
		slog.Warn("wardyn-proxy: dns rows over the bound", slog.Int("dropped", over), slog.Int("per_second", maxDNSRows))
	}
}

// dnsOwnName is the name the agent reaches this proxy by. Its /etc/hosts (the
// pod's HostAliases on Kubernetes, the container's extra hosts on Docker) holds
// only the IPv4 address, so the resolver asks DNS for the AAAA.
const dnsOwnName = "wardyn-proxy"

// isOwnDNSQuestion reports whether q is the one question the resolver asks that
// /etc/hosts cannot answer: AAAA, in the Internet class, for the proxy's exact
// name in any case, with or without the trailing dot. An A or any other type for
// that name, "wardyn-proxy.example.com" and "xwardyn-proxy" are not it.
func isOwnDNSQuestion(q dnsmessage.Question) bool {
	return q.Class == dnsmessage.ClassINET && q.Type == dnsmessage.TypeAAAA && strings.TrimSuffix(strings.ToLower(q.Name.String()), ".") == dnsOwnName
}

// answerDNS answers one DNS message, echoing its one question. A standard
// query (opcode 0) for AAAA of the proxy's own name gets NOERROR with no answer (the name exists, the
// type has no data), and record is false: it is not a lookup to audit. Every
// other query gets REFUSED and its row. A
// message that does not parse, is a response, or asks more or less than one
// question gets no reply and no row.
func answerDNS(msg []byte) (out []byte, q egress.DNSQuery, record, ok bool) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return nil, egress.DNSQuery{}, false, false
	}
	qs, err := p.AllQuestions()
	if err != nil || h.Response || len(qs) != 1 {
		return nil, egress.DNSQuery{}, false, false
	}
	own := h.OpCode == 0 && isOwnDNSQuestion(qs[0]) // a standard QUERY only: IQUERY, STATUS, NOTIFY and UPDATE are refused and recorded
	rcode := dnsmessage.RCodeRefused
	if own {
		rcode = dnsmessage.RCodeSuccess
	}
	// Authoritative on the empty answer only: without the AA bit a resolver reads NOERROR with no
	// answer as a lame referral, the same as REFUSED, and retries or fails the lookup instead of
	// taking NODATA (internal/vmnet/dns.go sets it the same way).
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: own, OpCode: h.OpCode, RecursionDesired: h.RecursionDesired, RCode: rcode})
	_ = b.StartQuestions()
	_ = b.Question(qs[0])
	out, err = b.Finish()
	if err != nil {
		return nil, egress.DNSQuery{}, false, false
	}
	if own {
		return out, egress.DNSQuery{}, false, true
	}
	q = egress.DNSQuery{Name: dnsName(qs[0].Name.String()), Type: dnsType(qs[0].Type.String()), Outcome: dnsRefused, Via: dnsViaProxy}
	if q.Name == "" {
		q.Cause = "name"
	}
	if q.Type == "" {
		q.Type = "OTHER"
	}
	return out, q, true, true
}

// ListenDNS opens the run's refusal-only DNS listener on addr, UDP and TCP,
// and serves it until ctx ends. It answers every query REFUSED and writes its
// row, except an AAAA for the proxy's own name, which gets an empty answer and
// no row; it never resolves a name and never forwards a query. With AllowedPeers
// set it serves only callers inside them, as the main listener does: a datagram
// from outside is dropped, and a connection from outside is closed. The error
// is the bind's.
func (s *Server) ListenDNS(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		_ = pc.Close()
		return err
	}
	// Serve only AllowedPeers when they are set, as the main listener does: on Docker this port is also
	// on the control network, where another container could otherwise write rows into the run.
	var served net.PacketConn = pc
	var lnServed net.Listener = ln
	if len(s.allowedPeers) > 0 {
		served = &peerPacketConn{PacketConn: pc, allowed: s.allowedPeers}
		lnServed = &peerListener{Listener: ln, allowed: s.allowedPeers}
	}
	go func() { _ = serveDNS(ctx, served, lnServed, newDNSLimiter(s.proxy.now, s.proxy.recordDNS)) }()
	return nil
}

func serveDNS(ctx context.Context, pc net.PacketConn, ln net.Listener, lim *dnsLimiter) error {
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(3)
	go func() {
		defer wg.Done()
		t := time.NewTicker(dnsInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				lim.flush()
			}
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if out, q, record, ok := answerDNS(buf[:n]); ok {
				_, _ = pc.WriteTo(out, from)
				if record {
					lim.note(q)
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		slots := make(chan struct{}, maxDNSTCPConns)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				_ = c.Close() // over the bound: the client sees a reset, as it would with no listener
				continue
			}
			go func() {
				defer func() { <-slots }()
				serveDNSConn(c, lim)
			}()
		}
	}()
	<-ctx.Done()
	close(stop)
	_ = pc.Close()
	_ = ln.Close()
	wg.Wait()
	return nil
}

// serveDNSConn answers RFC 7766 length-prefixed queries on one TCP connection
// until it idles or sends something that is not a query.
func serveDNSConn(c net.Conn, lim *dnsLimiter) {
	defer c.Close()
	var size [2]byte
	for {
		_ = c.SetDeadline(time.Now().Add(dnsTCPIdle))
		if _, err := io.ReadFull(c, size[:]); err != nil {
			return
		}
		msg := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(c, msg); err != nil {
			return
		}
		out, q, record, ok := answerDNS(msg)
		if !ok {
			return
		}
		if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(out))), out...)); err != nil {
			return
		}
		if record {
			lim.note(q)
		}
	}
}

// takesDNS reports whether the sink's reader writes DNS rows: the control
// plane's own endpoint, which upgrades before proxies do, and a runner's spool
// only once its accept says it replays them (an older runner's spool hash would
// refuse the row and stop replay, as with usage rows). A local spool never does.
func (s *decisionSink) takesDNS() bool {
	switch {
	case s == nil || s.local != nil:
		return false
	case s.remote != nil:
		return s.remote.dns.Load()
	}
	return true
}
