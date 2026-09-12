// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// privateIPMemoMax bounds the per-run memo. Its keys are host:port strings the
// SANDBOX chooses, so an agent looping over generated names is the same memory
// argument internal/api's authFailedLimiter makes about attacker-influenced
// keys: a map fed from the untrusted side needs a ceiling it cannot be pushed
// past. Small on purpose — a run's set of refused private endpoints is a handful
// — and evicting the wrong entry costs nothing but correctness-neutral work: its
// next attempt re-resolves and emits a fresh row, which is exactly the
// behaviour before this memo existed.
const privateIPMemoMax = 64

// privateIPMemo remembers, for the life of ONE run, that (host, port) was
// refused builtin:private-ip after resolution — and how many identical attempts
// have since been refused straight out of the memo.
//
// WHY THIS VERDICT AND NO OTHER (B6): the agent CLI retries a denial ten times
// (that loop is the CLI's — the proxy has none), and each retry used to
// re-resolve the name, re-vet it and emit another egress.deny, so a single
// misconfigured private endpoint filled the run's evidence rail with ten
// identical denials. The private-address guard is the one refusal that cannot
// change its mind mid-run: the internal_hosts lift that would lift it is
// compiled into this sidecar's config at dispatch and read once at startup, so
// no site-config change reaches the run being refused (the 403 body says so, in
// egressDenialSuffix). A builtin:resolve-failed is a DNS fault that may clear on
// the next attempt and an approval-pending hold is waiting for a human — both
// MUST keep asking, and neither is memoed. Step 0's literal-IP guard is not
// memoed either: it resolves nothing, so there is nothing to save.
//
// STATED CEILING (docs/OPERATIONS.md's Network section says this to operators):
// the memo is per run, so a name that flips from a private to a public address
// mid-run stays refused until the run ends. The remedy is the one the 403
// already gives — declare it under internal_hosts — and a new run re-resolves.
//
// ponytail: eviction linear-scans at most privateIPMemoMax entries for the
// oldest hit instead of maintaining an intrusive LRU list. At 64 entries the
// scan is cheaper than the bookkeeping; reach for container/list if the ceiling
// ever grows by an order of magnitude.
type privateIPMemo struct {
	mu sync.Mutex
	m  map[string]*privateIPStreak
}

// privateIPStreak is one memoed refusal: the request that earned it (so the
// summary row describes a real attempt rather than a synthesised one), how many
// identical attempts have been answered from the memo since, and when the last
// one arrived — which is both the summary row's timestamp and the eviction pick.
//
// The count is per (host, port), the granularity the guard's own verdict has, so
// a streak mixing CONNECT and GET against the same host:port reports the first
// attempt's method. Deliberate: the count answers "how many times did this
// refusal repeat", not "which verbs asked".
type privateIPStreak struct {
	req     egress.Request
	repeats int
	last    time.Time
	// kind is the guard class that refused this host:port — carried so
	// writeEgressDeny can tell a LIFTABLE refusal (RFC1918, one internal_hosts
	// line away) from one no site-config entry can lift, without resolving the
	// name a second time. The memo is already the "we refused this before" record
	// for exactly the verdict whose detail sentence needs it.
	kind blockKind
}

// record opens a streak for a fresh post-resolution private-ip refusal. It
// returns the summary row of an entry it had to EVICT to make room, if any, so
// a repeat count is never dropped on the floor — the caller emits it.
//
// RACE, documented because it is harmless and not worth a lock across the whole
// decision: two requests for the same (host, port) that BOTH miss the memo
// before either records will each resolve, each be refused, and each emit an
// opening row — two rows for one verdict instead of one. Every attempt after
// them is memoed. The lock here makes the MAP safe, not the check-then-act
// across evaluate(); widening it would serialise every denied request behind
// one mutex to save, at most, one duplicate row at the start of a streak. Over-
// reporting a denial is the safe direction for an audit trail.
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
// produces: a NEW egress.deny carrying the repeat count. Never an update of the
// row that opened the streak — the audit chain is append-only, and a decision
// already recorded is not rewritten because it happened again. A streak with no
// repeats produced no suppressed attempts and so produces no row.
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
//
// CALLED BEFORE THE FIRST-USE APPROVAL FLOW (V1-D5), not after it. It used to sit
// below, which meant a memoed host carrying an `unknown` policy verdict re-entered
// Resolve/ResolveWait on every one of the CLI's ten retries: a spent scope=once
// grant was consumed, or a fresh egress_domain question was POSTed to a human, or
// the connection was PARKED in ResolveWait until the hold deadline — and the
// request was then refused straight out of the memo with a NIL decision log, so a
// human decision was spent on a request denied with no decision row at all (before
// the memo existed it at least wrote an egress.deny). A private-IP target can
// never be approved into reachability: the address guard is unconditional and the
// internal_hosts lift that would change it is compiled into this sidecar at
// dispatch, so the question can only ever be answered "yes" and then overruled.
// Exactly the wasted question F032 moved the method check above the raise to stop.
//
// It stays BELOW policy:denied and policy:method: both name a more specific rule
// for this request, and neither costs a human anything.
func (p *Proxy) privateIPMemoHit(host string, port int) bool {
	return p.privIP.hit(host, port, p.now())
}

// privateIPMemoed reports a memoed refusal without counting it (writeEgressDeny).
func (p *Proxy) privateIPMemoed(host string, port int) bool {
	return p.privIP.memoed(host, port)
}

// privateIPBlockKind is writeEgressDeny's read of the guard class behind a
// builtin:private-ip refusal of host:port. See privateIPMemo.kindOf for why
// blockNone (never refused here — e.g. step 0's literal guard, which resolves
// nothing and is not memoed) is the safe answer: the literal arm re-derives the
// class from the address itself, and a hostname falls back to today's wording.
func (p *Proxy) privateIPBlockKind(host string, port int) blockKind {
	return p.privIP.kindOf(host, port)
}

// flushPrivateIPMemo emits every open streak's summary row and empties the memo.
// Called at run end (Server.Shutdown, before the decision sink drains) so a
// repeat count is not lost to the sandbox simply stopping.
//
// CEILING, stated rather than implied: this runs on the ORDERLY stop —
// cmd/wardyn-proxy's signal handler calls Shutdown with a 15 s budget, so a
// SIGTERM flushes. A SIGKILL at grace expiry, an OOM kill, or a pod deleted out
// from under the sidecar drops whatever streaks were still open. What is lost is
// only the repeat COUNT: the row that OPENED each streak was emitted when the
// refusal was first decided and is already in the trail, so a hard kill
// under-reports how many times a denial repeated and never loses the denial.
func (p *Proxy) flushPrivateIPMemo() {
	if p.sink == nil {
		return
	}
	for _, log := range p.privIP.drain() {
		p.sink.emit(log)
	}
}
