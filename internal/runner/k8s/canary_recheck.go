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
)

// Vars, not consts, only so tests can shorten them.
var (
	// canaryRecheckInterval is how often the egress canary re-runs in each runs namespace after boot. Fixed,
	// not a setting: a CNI change shows in the next check, and a restart checks at once.
	canaryRecheckInterval = 10 * time.Minute
)

// canaryRecheckStrikes is how many checks in a row must disagree with a namespace's verdict before it
// changes, so one flaky CNI answer or apiserver error never moves it. A check that proves enforcement
// changes it at once.
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
	// strikes counts the checks in a row whose result disagreed with state.
	strikes int
	// checkedAt is when the namespace's last check finished, whatever it found.
	checkedAt time.Time
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

// checkedAt is the oldest namespace's last check: every namespace was checked at least this recently.
func (v *netPolVerdict) checkedAt() time.Time {
	var oldest time.Time
	if v == nil {
		return oldest
	}
	for _, n := range v.byNS {
		if oldest.IsZero() || n.checkedAt.Before(oldest) {
			oldest = n.checkedAt
		}
	}
	return oldest
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
		ns, errNetPolNotProven, n.state, n.checkedAt.UTC().Format(time.RFC3339))
}

// startCanaryRecheck starts the loop that re-runs the canary in every runs namespace each
// canaryRecheckInterval. stopRecheck ends it.
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

// stopCanaryRecheck stops the re-check loop, if one runs, and waits for it.
func (d *Driver) stopCanaryRecheck() {
	if d.stopRecheck != nil {
		d.stopRecheck()
	}
}

// recheckOnce re-runs the canary in each runs namespace and publishes each result as it lands. The loop
// is the only writer. A check cut short by stopping is no evidence and changes nothing.
func (d *Driver) recheckOnce(ctx context.Context) {
	for _, ns := range d.cfg.Namespaces {
		verdict, err := d.runEgressCanary(ctx, ns)
		if ctx.Err() != nil {
			return
		}
		cur := d.netPol.Load()
		next := &netPolVerdict{byNS: make(map[string]nsVerdict, len(d.cfg.Namespaces))}
		if cur != nil {
			maps.Copy(next.byNS, cur.byNS)
		}
		next.byNS[ns] = d.advance(ns, next.byNS[ns], verdict, err)
		d.netPol.Store(next)
	}
}

// advance applies one check's result to a namespace's verdict: a result that agrees, or proves
// enforcement, settles it at once; any other must repeat canaryRecheckStrikes times in a row, and the
// last of them becomes the verdict (indeterminate ones as "not proven").
func (d *Driver) advance(ns string, prev nsVerdict, verdict canaryVerdict, err error) nsVerdict {
	got := stateOf(verdict)
	next := nsVerdict{state: prev.state, strikes: prev.strikes + 1, checkedAt: d.now()}
	if got == prev.state || got == netPolProven {
		next.state, next.strikes = got, 0
	} else if next.strikes >= canaryRecheckStrikes {
		next.state, next.strikes = got, 0
	}
	attrs := []any{slog.String("namespace", ns), slog.String("result", got.String()), slog.String("verdict", next.state.String()),
		slog.Time("checked_at", next.checkedAt)}
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
		slog.Warn("wardynd: k8s egress canary re-check disagreed with the NetworkPolicy verdict; it changes after repeated failures", attrs...)
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
