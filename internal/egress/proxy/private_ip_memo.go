// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// privateIPMemoMax bounds the per-run memo. Its keys are host:port strings the
// SANDBOX chooses, so — like internal/api's authFailedLimiter — a map fed from
// the untrusted side needs a ceiling it cannot be pushed past. Small on
// purpose; evicting the wrong entry costs nothing but a re-resolve and a fresh
// row, the pre-memo behaviour.
const privateIPMemoMax = 64

// privateIPMemo remembers, for the life of ONE run, that (host, port) was
// refused builtin:private-ip after resolution — and how many identical
// attempts have since been refused straight out of the memo. Without it, the
// agent CLI's ten retries of a denial would each re-resolve and re-emit an
// egress.deny, filling the run's evidence rail with duplicates. Only this
// verdict is memoed: it is the one refusal that cannot change its mind
// mid-run (the internal_hosts lift is compiled into the sidecar at dispatch),
// while a resolve failure or an approval-pending hold must keep asking.
//
// Stated ceiling (docs/OPERATIONS.md): the memo is per run, so a name that
// flips from private to public mid-run stays refused until the run ends;
// declare it under internal_hosts, and a new run re-resolves.
//
// ponytail: eviction linear-scans at most privateIPMemoMax entries for the
// oldest hit instead of maintaining an intrusive LRU list. At 64 entries the
// scan is cheaper than the bookkeeping; reach for container/list if the ceiling
// ever grows by an order of magnitude.
type privateIPMemo struct {
	mu sync.Mutex
	m  map[string]*privateIPStreak
}

// privateIPStreak is one memoed refusal: the request that earned it, how many
// identical attempts have been answered from the memo since, and when the last
// one arrived (both the summary row's timestamp and the eviction pick). The
// count is per (host, port): a streak mixing CONNECT and GET against the same
// host:port reports the first attempt's method, since the count answers "how
// many times did this repeat", not "which verbs asked".
type privateIPStreak struct {
	req     egress.Request
	repeats int
	last    time.Time
	// kind is the guard class that refused this host:port, carried so
	// writeEgressDeny can tell a LIFTABLE refusal from one nothing can lift
	// without resolving the name a second time.
	kind blockKind
}

// record opens a streak for a fresh post-resolution private-ip refusal. It
// returns the summary row of an entry it had to EVICT to make room, if any, so
// a repeat count is never dropped on the floor — the caller emits it.
//
// Harmless documented RACE: two requests for the same (host, port) that BOTH
// miss the memo before either records will each emit an opening row instead of
// one; every attempt after them is memoed. The lock here only makes the MAP
// safe, not the check-then-act across evaluate() — over-reporting a denial is
// the safe direction for an audit trail.
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

// hit reports whether (host, port) is already memoed and, when it is, counts
// this attempt against the streak. A true means the caller must neither
// re-resolve nor emit a second decision row.
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

// memoed is hit's read-only twin, for writeEgressDeny: it answers "is this
// refusal coming out of the memo" without counting the attempt a second time.
func (mo *privateIPMemo) memoed(host string, port int) bool {
	mo.mu.Lock()
	defer mo.mu.Unlock()
	return mo.m[hostPortKey(host, port)] != nil
}

// kindOf returns the guard class recorded for host:port, or blockNone when this
// memo has never refused it. blockNone makes literalIPDenialDetail fall back to
// the liftable wording, which is what every refusal got before the class was
// carried at all.
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

// closeStreak turns a closing streak into the ONE extra decision row it
// produces: a NEW egress.deny carrying the repeat count, never an update of the
// row that opened the streak (the audit chain is append-only). A streak with
// no repeats produces no row.
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
// Called before the first-use approval flow, not after: a private-IP target
// can never be approved into reachability, so calling it after would spend a
// human decision (or a scope=once grant) on a request the memo then refuses
// anyway with a nil decision log. It stays BELOW policy:denied and
// policy:method, which both name a more specific rule and cost a human nothing.
func (p *Proxy) privateIPMemoHit(host string, port int) bool {
	return p.privIP.hit(host, port, p.now())
}

// privateIPMemoed reports a memoed refusal without counting it (writeEgressDeny).
func (p *Proxy) privateIPMemoed(host string, port int) bool {
	return p.privIP.memoed(host, port)
}

// privateIPBlockKind is writeEgressDeny's read of the guard class behind a
// builtin:private-ip refusal of host:port. blockNone is the safe fallback for
// an unmemoed refusal (e.g. step 0's literal guard, which resolves nothing).
func (p *Proxy) privateIPBlockKind(host string, port int) blockKind {
	return p.privIP.kindOf(host, port)
}

// flushPrivateIPMemo emits every open streak's summary row and empties the
// memo. Called at run end (Server.Shutdown, before the decision sink drains) so
// a repeat count is not lost to the sandbox simply stopping.
//
// Ceiling: this only runs on the ORDERLY stop (SIGTERM, 15s budget); a SIGKILL,
// OOM kill or deleted pod drops whatever streaks were still open. Only the
// repeat COUNT is lost then — the opening row was already emitted when the
// refusal was first decided, so a hard kill under-reports repeats and never
// loses the denial itself.
func (p *Proxy) flushPrivateIPMemo() {
	if p.sink == nil {
		return
	}
	for _, log := range p.privIP.drain() {
		p.sink.emit(log)
	}
}
