// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"log/slog"
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
	count int
}

type reporter struct {
	mu   sync.Mutex
	now  func() time.Time
	last map[string]time.Time
	held map[string]*held
	send func(refusal)
	log  *slog.Logger
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
	for k, h := range rp.held {
		if now.Sub(rp.last[k]) >= reportInterval {
			r := h.r
			r.Repeat = h.count - 1 // h.r itself is the row; the rest are its repeats
			out = append(out, r)
			rp.last[k] = now
			delete(rp.held, k)
		}
	}
	rp.mu.Unlock()
	for _, r := range out {
		rp.emit(r)
	}
}

func (rp *reporter) emit(r refusal) {
	rp.log.Info("deny", "reason", r.Reason, "flow", r.Flow, "host", r.Host, "port", r.Port, "detail", r.Detail, "repeat", r.Repeat)
	go rp.send(r)
}

// refuseLocal logs a refusal that makes no row (a DNS name refused for its
// spelling or a full table), rate limited like the rest.
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
