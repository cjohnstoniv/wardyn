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
//     which answers every query REFUSED: those sandboxes had no DNS before it.
const (
	routeVMNetDNS = "/wardyn/v1/vmnet/dns"

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

// dnsName keeps a lower-case LDH name of at most 253 bytes, or gives "".
func dnsName(s string) string {
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	if net.ParseIP(s) != nil {
		return "" // a name, never an address
	}
	return vmnetName(s)
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
		if now.Sub(l.last[k]) < dnsInterval || !l.take(now) {
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

// refuseDNS answers one DNS message REFUSED, echoing its one question, and
// gives the row for it. A message that does not parse, is a response, or asks
// more or less than one question gets no reply and no row.
func refuseDNS(msg []byte) ([]byte, egress.DNSQuery, bool) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return nil, egress.DNSQuery{}, false
	}
	qs, err := p.AllQuestions()
	if err != nil || h.Response || len(qs) != 1 {
		return nil, egress.DNSQuery{}, false
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, OpCode: h.OpCode, RecursionDesired: h.RecursionDesired, RCode: dnsmessage.RCodeRefused})
	_ = b.StartQuestions()
	_ = b.Question(qs[0])
	out, err := b.Finish()
	if err != nil {
		return nil, egress.DNSQuery{}, false
	}
	q := egress.DNSQuery{Name: dnsName(qs[0].Name.String()), Type: dnsType(qs[0].Type.String()), Outcome: dnsRefused, Via: dnsViaProxy}
	if q.Name == "" {
		q.Cause = "name"
	}
	if q.Type == "" {
		q.Type = "OTHER"
	}
	return out, q, true
}

// ServeDNS runs the run's refusal-only DNS listener on addr, UDP and TCP, until
// ctx ends. It answers every query REFUSED and writes its row; it never
// resolves a name and never forwards a query.
func (s *Server) ServeDNS(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		_ = pc.Close()
		return err
	}
	return serveDNS(ctx, pc, ln, newDNSLimiter(s.proxy.now, s.proxy.recordDNS))
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
			if out, q, ok := refuseDNS(buf[:n]); ok {
				_, _ = pc.WriteTo(out, from)
				lim.note(q)
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
		out, q, ok := refuseDNS(msg)
		if !ok {
			return
		}
		if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(out))), out...)); err != nil {
			return
		}
		lim.note(q)
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
