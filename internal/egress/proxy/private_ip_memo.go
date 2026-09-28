// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// privateIPMemoMax bounds the per-run memo: keys come from the SANDBOX-chosen
// host:port, so this untrusted-fed map needs a hard ceiling. Small on purpose —
// evicting early just costs a re-resolve.
const privateIPMemoMax = 64

// privateIPMemo remembers, for one run, that (host, port) was refused
// builtin:private-ip after resolution, plus how many identical repeats
// followed — without it, the agent CLI's retries would each re-resolve and
// duplicate the egress.deny. Only this verdict is memoed since it can't change
// mid-run; a resolve failure or approval-pending hold must keep asking.
//
// Ceiling (docs/OPERATIONS.md): per-run scope means a name that turns public
// mid-run stays refused until the run ends.
//
// ponytail: eviction linear-scans for the oldest entry instead of an LRU list;
// cheaper than the bookkeeping at this size.
type privateIPMemo struct {
	mu sync.Mutex
	m  map[string]*privateIPStreak
}

// privateIPStreak is one memoed refusal: the request, repeat count, and
// last-seen time (used both for the summary row and eviction). Count is per
// (host, port); a mixed-method streak reports the first method, since only
// the repeat count matters.
type privateIPStreak struct {
	req     egress.Request
	repeats int
	last    time.Time
	// kind is the guard class that refused this host:port, so writeEgressDeny
	// can tell a liftable refusal from one that needs a fresh resolve.
	kind blockKind
}

// record opens a streak for a fresh post-resolution refusal, returning the
// summary row of any evicted entry so the caller can emit it.
//
// Harmless RACE: concurrent misses on the same (host, port) can each open a
// streak before either records. The mutex only protects the map, not the
// check-then-act across evaluate() — over-reporting a denial is the safe
// direction for an audit trail.
func (mo *privateIPMemo) record(req egress.Request, kind blockKind) *egress.DecisionLog {
	mo.mu.Lock()
	defer mo.mu.Unlock()
	if mo.m == nil {
		mo.m = make(map[string]*privateIPStreak, privateIPMemoMax)
	}
	key := hostPortKey(req.Host, req.Port)
	if _, ok := mo.m[key]; ok {
		return nil
	}
	var evicted *egress.DecisionLog
	if len(mo.m) >= privateIPMemoMax {
		oldestKey := ""
		for k, s := range mo.m {
			if oldestKey == "" || s.last.Before(mo.m[oldestKey].last) {
				oldestKey = k
			}
		}
		evicted = closeStreak(mo.m[oldestKey])
		delete(mo.m, oldestKey)
	}
	mo.m[key] = &privateIPStreak{req: req, last: req.Time, kind: kind}
	return evicted
}

// hit reports whether (host, port) is memoed, counting this attempt if so.
// True means the caller must not re-resolve or emit a second decision row.
func (mo *privateIPMemo) hit(host string, port int, now time.Time) bool {
	mo.mu.Lock()
	defer mo.mu.Unlock()
	s := mo.m[hostPortKey(host, port)]
	if s == nil {
		return false
	}
	s.repeats++
	s.last = now
	return true
}

// memoed is hit's read-only twin: reports a memoed refusal without counting it.
func (mo *privateIPMemo) memoed(host string, port int) bool {
	mo.mu.Lock()
	defer mo.mu.Unlock()
	return mo.m[hostPortKey(host, port)] != nil
}

// kindOf returns the guard class recorded for host:port, or blockNone when
// never refused — the safe fallback used before the class was tracked at all.
func (mo *privateIPMemo) kindOf(host string, port int) blockKind {
	mo.mu.Lock()
	defer mo.mu.Unlock()
	if s := mo.m[hostPortKey(host, port)]; s != nil {
		return s.kind
	}
	return blockNone
}

// drain closes and removes every open streak, returning their summary rows.
func (mo *privateIPMemo) drain() []egress.DecisionLog {
	mo.mu.Lock()
	defer mo.mu.Unlock()
	var out []egress.DecisionLog
	for k, s := range mo.m {
		if log := closeStreak(s); log != nil {
			out = append(out, *log)
		}
		delete(mo.m, k)
	}
	return out
}

// closeStreak turns a closing streak into one new egress.deny row carrying
// the repeat count — never an update, since the audit chain is append-only.
// No repeats, no row.
func closeStreak(s *privateIPStreak) *egress.DecisionLog {
	if s == nil || s.repeats == 0 {
		return nil
	}
	log := decisionLog(s.req, egress.Deny, "builtin:private-ip")
	log.Request.Time = s.last
	log.Repeat = s.repeats
	return &log
}

// privateIPRefused opens a memo streak for a refusal evaluate() has just
// recorded, emitting the summary row of whatever it evicted to make room.
func (p *Proxy) privateIPRefused(req egress.Request, kind blockKind) {
	if evicted := p.privIP.record(req, kind); evicted != nil && p.sink != nil {
		p.sink.emit(*evicted)
	}
}

// privateIPMemoHit answers an identical repeat from the memo, counting it.
// Must run before first-use approval: a private-IP target can never become
// reachable, so approving first would spend a human decision (or scope=once
// grant) on a request the memo refuses anyway.
func (p *Proxy) privateIPMemoHit(host string, port int) bool {
	return p.privIP.hit(host, port, p.now())
}

// privateIPMemoed reports a memoed refusal without counting it (writeEgressDeny).
func (p *Proxy) privateIPMemoed(host string, port int) bool {
	return p.privIP.memoed(host, port)
}

// privateIPBlockKind is writeEgressDeny's read of the guard class behind a
// builtin:private-ip refusal; blockNone is the safe fallback for an unmemoed
// refusal (e.g. step 0's literal guard, which resolves nothing).
func (p *Proxy) privateIPBlockKind(host string, port int) blockKind {
	return p.privIP.kindOf(host, port)
}

// flushPrivateIPMemo emits every open streak's summary row and empties the
// memo, called at run end (before the decision sink drains) so a repeat count
// isn't lost when the sandbox simply stops.
//
// Ceiling: only runs on orderly stop (SIGTERM, 15s budget); a SIGKILL, OOM
// kill or deleted pod loses open repeat counts but never the denial itself,
// since the opening row was already emitted.
func (p *Proxy) flushPrivateIPMemo() {
	if p.sink == nil {
		return
	}
	for _, log := range p.privIP.drain() {
		p.sink.emit(log)
	}
}
