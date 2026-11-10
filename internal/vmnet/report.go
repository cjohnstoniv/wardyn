// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A hostile or merely chatty guest can make a refusal per frame (an idle
// Debian guest sends an IPv6 frame every few seconds), so refusals are rate
// limited per kind before they reach the relay's log or the run's decision log:
// the first of a kind in each interval goes out at once, the rest are counted,
// and the count rides the next report of that kind or the periodic flush.
// A refusal of an asked flow is never held: it ends a flow the proxy asked
// about and parked, and it is that flow's one row. Each such flow cost the
// guest a proxy decision, so these are bounded by the proxy, not by this limit.
// Every other refusal, a port-80 flow's not-http or host-mismatch included
// (made before any ask), is limited per kind.
const reportInterval = time.Second

type held struct {
	r     refusal
	q     *dnsRecord // a held DNS record instead of a refusal
	count int
}

type reporter struct {
	mu   sync.Mutex
	now  func() time.Time
	last map[string]time.Time
	held map[string]*held
	send func(refusal)
	log  *slog.Logger

	// sendDNS reports one DNS record; nil sends none (the relay's log line has it).
	sendDNS func(dnsRecord)
	// dnsFrom and dnsSent count the DNS records sent since dnsFrom, which
	// maxDNSReports bounds; dnsCapped counts the records over it.
	dnsFrom   time.Time
	dnsSent   int
	dnsCapped int
}

// The DNS record kinds: every query the relay answers is one of these. Only
// the relay's name table answers; nothing is looked up upstream.
const (
	dnsAnswered    = "answered"     // a synthetic A answer, or no data for another type
	dnsRefused     = "refused"      // REFUSED: Cause "name" (spelling) or "full" (name table)
	dnsPrepRefused = "prep_refused" // a preparation relay's refuse-all

	dnsPath = "/wardyn/v1/vmnet/dns"

	// maxDNSReports bounds the DNS records sent per reportInterval across all
	// names: the per-(name, type, outcome) limit alone lets a guest that asks
	// for a new name each time send a record each time.
	maxDNSReports = 64
)

// dnsRecord is the record of one guest DNS query for the run's audit
// (internal/egress/proxy/dns_audit.go). Name is empty when the name's spelling
// was refused; Answer is the synthetic address of an answered A query.
type dnsRecord struct {
	Name    string `json:"name,omitempty"`
	Type    string `json:"type"`
	Outcome string `json:"outcome"`
	Cause   string `json:"cause,omitempty"`
	Answer  string `json:"answer,omitempty"`
	Repeat  int    `json:"repeat,omitempty"`
}

func (q dnsRecord) key() string {
	return "dns\x00" + q.Name + "\x00" + q.Type + "\x00" + q.Outcome + "\x00" + q.Cause
}

// dns records one guest DNS query, rate limited per (name, type, outcome) like
// a refusal kind: the first in each interval goes out, the rest are counted on
// the next. The query was already answered; this only reports it.
func (rp *reporter) dns(q dnsRecord) {
	k := q.key()
	rp.mu.Lock()
	now := rp.now()
	if now.Sub(rp.last[k]) < reportInterval {
		h := rp.held[k]
		if h == nil {
			h = &held{}
			rp.held[k] = h
		}
		h.q, h.count = &q, h.count+1
		rp.mu.Unlock()
		return
	}
	if !rp.takeDNS(now) {
		rp.mu.Unlock()
		return
	}
	rp.last[k] = now
	if h := rp.held[k]; h != nil {
		q.Repeat = h.count
		delete(rp.held, k)
	}
	rp.mu.Unlock()
	rp.emitDNS(q)
}

// takeDNS spends one of the interval's DNS records, or counts one over the
// bound. Called with rp.mu held.
func (rp *reporter) takeDNS(now time.Time) bool {
	if now.Sub(rp.dnsFrom) >= reportInterval {
		rp.dnsFrom, rp.dnsSent = now, 0
	}
	if rp.dnsSent >= maxDNSReports {
		rp.dnsCapped++
		return false
	}
	rp.dnsSent++
	return true
}

func (rp *reporter) emitDNS(q dnsRecord) {
	rp.log.Info("dns", "name", q.Name, "type", q.Type, "outcome", q.Outcome, "cause", q.Cause, "answer", q.Answer, "repeat", q.Repeat)
	if rp.sendDNS != nil {
		go rp.sendDNS(q)
	}
}

// sendDNS posts one DNS record. Best effort, like send.
func (c *proxyClient) sendDNS(q dnsRecord) error {
	body, _ := json.Marshal(q)
	resp, err := c.report.Post("http://"+c.addr+dnsPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("vmnet: dns report answered %d", resp.StatusCode)
	}
	return nil
}

func newReporter(now func() time.Time, log *slog.Logger, send func(refusal)) *reporter {
	return &reporter{now: now, log: log, send: send, last: map[string]time.Time{}, held: map[string]*held{}}
}

// key is the rate-limit kind: the reason, plus the drop kind for frames.
func key(r refusal) string {
	if r.Reason == "dropped-frame" {
		return r.Reason + "/" + r.Detail
	}
	return r.Reason
}

// refuse records one refusal: reported now, or held for the next report.
func (rp *reporter) refuse(r refusal) {
	if r.asked {
		rp.emit(r)
		return
	}
	k := key(r)
	rp.mu.Lock()
	now := rp.now()
	if now.Sub(rp.last[k]) < reportInterval {
		h := rp.held[k]
		if h == nil {
			h = &held{}
			rp.held[k] = h
		}
		h.r, h.count = r, h.count+1
		rp.mu.Unlock()
		return
	}
	rp.last[k] = now
	if h := rp.held[k]; h != nil {
		r.Repeat = h.count
		delete(rp.held, k)
	}
	rp.mu.Unlock()
	rp.emit(r)
}

// flush reports every held count whose interval has passed.
func (rp *reporter) flush() {
	rp.mu.Lock()
	now := rp.now()
	var out []refusal
	var outDNS []dnsRecord
	for k, h := range rp.held {
		if now.Sub(rp.last[k]) < reportInterval {
			continue
		}
		if h.q != nil {
			if !rp.takeDNS(now) {
				continue // still held: its count rides a later flush
			}
			q := *h.q
			q.Repeat = h.count - 1
			outDNS = append(outDNS, q)
		} else {
			r := h.r
			r.Repeat = h.count - 1 // h.r itself is the row; the rest are its repeats
			out = append(out, r)
		}
		rp.last[k] = now
		delete(rp.held, k)
	}
	// A DNS key is per name, so the guest picks how many there are: forget
	// one whose interval has passed with nothing held.
	for k, t := range rp.last {
		if strings.HasPrefix(k, "dns\x00") && now.Sub(t) >= reportInterval && rp.held[k] == nil {
			delete(rp.last, k)
		}
	}
	capped := rp.dnsCapped
	rp.dnsCapped = 0
	rp.mu.Unlock()
	for _, r := range out {
		rp.emit(r)
	}
	for _, q := range outDNS {
		rp.emitDNS(q)
	}
	if capped > 0 {
		rp.log.Warn("dns reports over the bound", "dropped", capped, "per_second", maxDNSReports)
	}
}

func (rp *reporter) emit(r refusal) {
	rp.log.Info("deny", "reason", r.Reason, "flow", r.Flow, "host", r.Host, "port", r.Port, "detail", r.Detail, "repeat", r.Repeat)
	go rp.send(r)
}

// refuseLocal logs a refusal that makes no row (a guest's declined DHCP
// lease), rate limited like the rest.
func (rp *reporter) refuseLocal(msg string, args ...any) {
	rp.mu.Lock()
	now := rp.now()
	if now.Sub(rp.last[msg]) < reportInterval {
		rp.mu.Unlock()
		return
	}
	rp.last[msg] = now
	rp.mu.Unlock()
	rp.log.Info(msg, args...)
}
