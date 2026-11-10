// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// Vars, not consts, only so tests can shorten them.
var (
	// canaryRecheckInterval is how often the egress canary re-runs in each runs namespace after boot. Fixed,
	// not a setting: a CNI change shows in the next check, and a restart checks at once.
	canaryRecheckInterval = 10 * time.Minute
)

// canaryRecheckStrikes is how many non-definite checks in a row (an apiserver error, a pod that never
// ran, or an acknowledged result after a definite one) move a namespace's verdict, so one flaky check
// never moves it. A definite result changes it at once.
const canaryRecheckStrikes = 3

// errNetPolNotProven: the live verdict no longer lets a new sandbox start in the namespace.
var errNetPolNotProven = errors.New("k8s: the egress canary does not prove NetworkPolicy enforced in this namespace")

// netPolState is one runs namespace's NetworkPolicy verdict.
type netPolState int

const (
	netPolNotProven    netPolState = iota // zero value, fail closed: the canary could not tell
	netPolProven                          // a deny-all policy blocked the connect
	netPolUnenforced                      // the connect got through a deny-all policy
	netPolAcknowledged                    // the operator accepted an ambient default-deny; never proof
)

func (s netPolState) String() string {
	switch s {
	case netPolProven:
		return "enforced"
	case netPolUnenforced:
		return "not enforced"
	case netPolAcknowledged:
		return "acknowledged, not proven"
	}
	return "not proven"
}

// stateOf maps one canary run's outcome to the state it would set.
func stateOf(v canaryVerdict) netPolState {
	switch v {
	case canaryEnforced:
		return netPolProven
	case canaryUnenforced:
		return netPolUnenforced
	case canaryAcknowledged:
		return netPolAcknowledged
	}
	return netPolNotProven
}

// nsVerdict is one namespace's live verdict.
type nsVerdict struct {
	state netPolState
	// strikes counts the indeterminate checks in a row since the last definite one.
	strikes int
	// escaped: the last definite result showed egress through a deny-all policy. It outlives the
	// "not enforced" state itself, so no result but proof of enforcement lifts the refusal.
	escaped bool
	// verdictAt is when the check that produced state finished: the latest check that agreed with it,
	// or the one that changed it. A disagreeing check that has not changed it yet never moves it.
	verdictAt time.Time
}

// netPolVerdict is the live verdict across the runs namespaces. Replaced whole, never changed in place.
type netPolVerdict struct {
	byNS map[string]nsVerdict
}

// enforced: every runs namespace proved NetworkPolicy enforced. A nil verdict proves nothing.
func (v *netPolVerdict) enforced() bool {
	if v == nil || len(v.byNS) == 0 {
		return false
	}
	for _, n := range v.byNS {
		if n.state != netPolProven {
			return false
		}
	}
	return true
}

// acknowledged: some namespace stands on the operator's acknowledgment and none is worse, so a failed
// re-check elsewhere never reads as the milder "acknowledged".
func (v *netPolVerdict) acknowledged() bool {
	if v == nil {
		return false
	}
	acked := false
	for _, n := range v.byNS {
		switch n.state {
		case netPolAcknowledged:
			acked = true
		case netPolProven:
		default:
			return false
		}
	}
	return acked
}

// netPolCheck is the live verdict as health reads it: the worst namespace's, with the time of the check
// that produced it.
func (d *Driver) netPolCheck() runner.NetPolCheck {
	var out runner.NetPolCheck
	if v := d.netPol.Load(); v != nil {
		for _, n := range v.byNS {
			out = runner.WorseNetPolCheck(out, runner.NetPolCheck{Verdict: d.wireVerdict(n.state), At: n.verdictAt})
		}
	}
	return out
}

// wireVerdict names a namespace state for health by what the driver does with it. Not enforced splits
// on whether new sandboxes still start, which the override allows for plain sandboxes only, never in
// agent-sandbox mode. Acknowledged proves nothing, so agent-sandbox mode refuses on it: there it reads
// "not proven" (refused until a check proves enforcement), never the milder warning, and never
// "refused", which claims a check showed egress getting through.
func (d *Driver) wireVerdict(s netPolState) string {
	switch s {
	case netPolProven:
		return runner.NetPolEnforced
	case netPolAcknowledged:
		if d.refuses(s) {
			return runner.NetPolNotProven
		}
		return runner.NetPolAcknowledged
	case netPolUnenforced:
		if d.refuses(s) {
			return runner.NetPolRefused
		}
		return runner.NetPolUnenforced
	}
	return runner.NetPolNotProven
}

// netPolRefusal says why a new sandbox may not start in ns under the live verdict, or nil. Running
// sandboxes are never touched. A plain sandbox runs on a proven, acknowledged, or (with
// WARDYN_K8S_ALLOW_UNENFORCED_NETPOL) unenforced namespace, the same as boot allows; an agent-sandbox
// one only on a proven namespace, whatever the override says.
func (d *Driver) netPolRefusal(ns string) error {
	v := d.netPol.Load()
	var n nsVerdict
	ok := v != nil
	if ok {
		n, ok = v.byNS[ns]
	}
	if !ok {
		return fmt.Errorf("k8s: refusing a new sandbox in namespace %q: %w (it was never checked)", ns, errNetPolNotProven)
	}
	if !d.refuses(n.state) {
		return nil
	}
	return fmt.Errorf("k8s: refusing a new sandbox in namespace %q: %w (%s, checked %s); running sandboxes keep running",
		ns, errNetPolNotProven, n.state, n.verdictAt.UTC().Format(time.RFC3339))
}

// startCanaryRecheck starts the loop that re-runs the canary in every runs namespace each
// canaryRecheckInterval. Close ends it.
func (d *Driver) startCanaryRecheck() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d.stopRecheck = func() {
		cancel()
		<-done
	}
	interval := canaryRecheckInterval
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.recheckOnce(ctx)
			}
		}
	}()
}

// Close stops the canary re-check loop, if one runs, and waits for a check in flight to remove its
// canary pod and policy. The driver's owner calls it at shutdown: wardynd through the orchestrator's
// Close, wardyn-runnerd when its runtime closes. Calling it again does nothing more.
func (d *Driver) Close() error {
	if d.stopRecheck != nil {
		d.stopRecheck()
	}
	return nil
}

// OnNetPolChange sets the hook the re-check loop calls each time the live verdict health reads
// (netPolCheck: the verdict, or the time of the check behind it) changes. wardyn-runnerd sends its caps
// again from it, so the organisation sees a re-check's verdict mid-session. The hook runs on the loop and
// must not block.
func (d *Driver) OnNetPolChange(f func()) {
	d.netPolChanged.Store(&f)
}

// recheckOnce re-runs the canary in each runs namespace and publishes each result as it lands. The loop
// is the only writer. A check cut short by stopping is no evidence and changes nothing.
func (d *Driver) recheckOnce(ctx context.Context) {
	for _, ns := range d.cfg.Namespaces {
		verdict, err := d.runEgressCanary(ctx, ns)
		if ctx.Err() != nil {
			return
		}
		before := d.netPolCheck()
		cur := d.netPol.Load()
		next := &netPolVerdict{byNS: make(map[string]nsVerdict, len(d.cfg.Namespaces))}
		if cur != nil {
			maps.Copy(next.byNS, cur.byNS)
		}
		next.byNS[ns] = d.advance(ns, next.byNS[ns], verdict, err)
		d.netPol.Store(next)
		if after := d.netPolCheck(); after.Verdict != before.Verdict || !after.At.Equal(before.At) {
			if f := d.netPolChanged.Load(); f != nil {
				(*f)()
			}
		}
	}
}

// advance applies one check's result to a namespace's verdict, stamping the time of the check that
// produced it. A definite result applies at once: a deny-all policy that leaks is proof the boundary is
// gone, so new sandboxes stop with the first such check, and proof of enforcement restores them.
//
// Anything else is debounced: it keeps the verdict, and its time, until canaryRecheckStrikes of them in
// a row move it. An indeterminate check then makes it "not proven". An acknowledged result (phase A's
// pod ran and exited 1, which a pod-network blip produces as well as an ambient default-deny) proves
// nothing either way, so it is applied at once only where nothing definite stands; after a proven
// verdict the strikes make it acknowledged, and after an escape "not proven", where it stays: an
// escape is kept on record (escaped) until proof of enforcement, the only result that lifts it.
func (d *Driver) advance(ns string, prev nsVerdict, verdict canaryVerdict, err error) nsVerdict {
	got, now := stateOf(verdict), d.now()
	// An escape stays on record through every result but proof of enforcement.
	escaped := prev.escaped || prev.state == netPolUnenforced
	next := nsVerdict{state: prev.state, verdictAt: prev.verdictAt, escaped: escaped}
	definite := prev.state == netPolProven || escaped // the verdict stands on a definite result
	switch {
	case verdict == canaryEnforced:
		next.state, next.verdictAt, next.escaped = netPolProven, now, false
	case verdict == canaryUnenforced:
		next.state, next.verdictAt, next.escaped = netPolUnenforced, now, true
	case verdict == canaryAcknowledged && !definite:
		next.state, next.verdictAt = netPolAcknowledged, now
	case prev.state == netPolNotProven && (verdict == canaryIndeterminate || escaped):
		next.verdictAt = now // still not proven; after an escape an acknowledged result changes nothing either
	default: // a non-definite result against a standing verdict: count it
		if next.strikes = prev.strikes + 1; next.strikes >= canaryRecheckStrikes {
			next.state, next.strikes, next.verdictAt = netPolNotProven, 0, now
			if verdict == canaryAcknowledged && prev.state == netPolProven {
				next.state = netPolAcknowledged
			}
		}
	}
	attrs := []any{slog.String("namespace", ns), slog.String("result", got.String()), slog.String("verdict", next.state.String()),
		slog.Time("checked_at", now)}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	switch {
	case next.state != prev.state && d.refuses(next.state):
		slog.Error("wardynd: k8s egress canary re-check changed the NetworkPolicy verdict; new sandboxes in this namespace are refused, running ones keep running", attrs...)
	case next.state != prev.state:
		slog.Warn("wardynd: k8s egress canary re-check changed the NetworkPolicy verdict", attrs...)
	case next.strikes > 0:
		attrs = append(attrs, slog.Int("failed_checks", next.strikes), slog.Int("of", canaryRecheckStrikes))
		slog.Warn("wardynd: k8s egress canary re-check proved nothing either way; the verdict moves only after repeated such checks", attrs...)
	}
	return next
}

// refuses reports whether a namespace in state s refuses new sandboxes.
func (d *Driver) refuses(s netPolState) bool {
	switch s {
	case netPolProven:
		return false
	case netPolAcknowledged:
		return d.cfg.AgentSandbox
	case netPolUnenforced:
		return !d.cfg.AllowUnenforcedNetPol || d.cfg.AgentSandbox
	}
	return true
}
