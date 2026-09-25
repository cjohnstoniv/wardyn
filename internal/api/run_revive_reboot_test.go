// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// startingRunner is reviveRunner that can start a kept agent again. It
// records how many proxies had been replaced when each start came, so a test
// can see the agent was never started ahead of its new proxy.
type startingRunner struct {
	*reviveRunner
	startedAfter []int
	startErr     error
}

func (r *startingRunner) StartSandbox(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startedAfter = append(r.startedAfter, len(r.replaced))
	return r.startErr
}

func (r *startingRunner) starts() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.startedAfter)
}

// newRebootFixture is newReviveFixture's run lost to a reboot instead: the
// watcher found its agent exited but still there, and the run was kept with
// its agent and proxy stopped.
func newRebootFixture(t *testing.T) (*reviveFixture, *startingRunner) {
	t.Helper()
	f := newReviveFixture(t)
	f.st.run.LostAt, f.st.run.LostReason = nil, ""
	f.ls.lapsed, f.ls.claimed = false, false
	f.sweepWatchers(t)
	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostReboot {
		t.Fatalf("fixture: lost = %v %q, want lost (reboot)", lostAt, reason)
	}
	f.run = f.st.run
	sr := &startingRunner{reviveRunner: f.rr}
	f.srv.cfg.Runner = sr
	return f, sr
}

// TestReviveRun_AfterAReboot is RL-11's security core. A run lost to a
// reboot gets its new proxy (fresh token, the owner's current denies) and only
// then its agent started again, so the agent's first byte out goes through the
// rewritten config and never through the old proxy or none. Audited as from
// reboot, with the agent started.
func TestReviveRun_AfterAReboot(t *testing.T) {
	f, sr := newRebootFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	cfg := f.newConfig(t)
	if !slices.Contains(cfg.Policy.DeniedDomains, "api.openai.com") || cfg.RunToken == "old-token" {
		t.Errorf("denied_domains %v, token %q; want the owner's current deny and a fresh token", cfg.Policy.DeniedDomains, cfg.RunToken)
	}
	if got := sr.starts(); !slices.Equal(got, []int{1}) {
		t.Fatalf("StartSandbox calls, by proxies replaced before each = %v; want one start, after the new proxy", got)
	}
	if lostAt, _ := f.st.lost(); lostAt != nil || f.st.State() != types.RunRunning {
		t.Errorf("lost %v, state %s; want the run live and RUNNING", lostAt, f.st.State())
	}
	ev := f.audit.eventsFor(f.run.ID, "run.revive")
	if len(ev) != 1 || ev[0].Outcome != "success" {
		t.Fatalf("run.revive events = %+v, want one success", ev)
	}
	if data := leaseAuditData(t, ev[0]); data["from"] != "reboot" || data["agent_started"] != true || data["subject"] != f.run.CreatedBy {
		t.Errorf("run.revive data = %v; want from reboot, agent_started, the owner as subject", data)
	}
}

// TestReviveRun_AfterARebootFailsClosed: a rebooted run whose new proxy or
// agent does not come up is lost (reboot) again with its agent and proxy
// stopped, never left running outside the lost and lease machinery. A proxy
// that could not be replaced never has the agent started behind it.
func TestReviveRun_AfterARebootFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange    func(f *reviveFixture, sr *startingRunner)
		wantStarts int
	}{
		"the proxy is not replaced": {func(f *reviveFixture, _ *startingRunner) {
			f.rr.replaceErr = errors.Join(runner.ErrProxyReplaceFailed, errors.New("docker: start proxy: boom"))
		}, 0},
		"the proxy image cannot be pulled": {func(f *reviveFixture, _ *startingRunner) {
			f.rr.replaceErr = errors.New("docker: pull wardyn-proxy: denied")
		}, 0},
		"the agent does not start": {func(_ *reviveFixture, sr *startingRunner) {
			sr.startErr = errors.New("docker: start agent: boom")
		}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			f, sr := newRebootFixture(t)
			tc.arrange(f, sr)
			ends := f.rn.endCount()
			if code := f.revive(t); code != http.StatusBadGateway {
				t.Fatalf("revive: code %d, want 502", code)
			}
			if got := sr.starts(); len(got) != tc.wantStarts {
				t.Errorf("StartSandbox calls = %v, want %d: an agent must never start behind a proxy that was not replaced", got, tc.wantStarts)
			}
			if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostReboot || f.st.State() != types.RunRunning {
				t.Errorf("lost = %v %q, state %s; want kept, lost (reboot) again, so the next revive starts its agent", lostAt, reason, f.st.State())
			}
			if f.rn.endCount() != ends+1 {
				t.Errorf("EndSandbox +%d, want +1: the agent and any new proxy must be stopped", f.rn.endCount()-ends)
			}
			ev := f.audit.eventsFor(f.run.ID, "run.revive")
			if len(ev) != 1 || ev[0].Outcome != "failure" || leaseAuditData(t, ev[0])["lost_again"] != true {
				t.Errorf("run.revive events = %+v, want one failure with lost_again", ev)
			}
		})
	}
}

// TestReviveRun_TheBulkRestartNeverStartsAnAgent: "Restart with current
// limits" is proxy-only. A run lost to a reboot is reported and left as it
// was; only its own page revives it.
func TestReviveRun_TheBulkRestartNeverStartsAnAgent(t *testing.T) {
	f, sr := newRebootFixture(t)
	w := do(t, f.srv, http.MethodPost, "/api/v1/admin/runs/restart", adminToken, `{"run_ids":["`+f.run.ID.String()+`"]}`)
	var out struct {
		Results []adminRestartResult `json:"results"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Results) != 1 || out.Results[0].OK {
		t.Fatalf("restart: code %d body %s; want the rebooted run refused", w.Code, w.Body.String())
	}
	if len(sr.starts()) != 0 || len(f.rr.replaced) != 0 {
		t.Errorf("StartSandbox %v, ReplaceProxy %d; want neither", sr.starts(), len(f.rr.replaced))
	}
	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostReboot {
		t.Errorf("lost = %v %q; want the run left lost (reboot)", lostAt, reason)
	}
}

// TestReviveRun_AfterAnExtension is F1's fix (long-holds design rev 4 §2.3,
// §4.1): an outage run's agent is stopped too once its end passes while it
// is still lost (run_lost.go stopLostSandbox), but lost_reason stays
// outage — relabeling would add a second writer racing the sweeps that set
// it. Extending the end is how such a run becomes revivable again, and the
// revive must not trust the outage label alone: it probes the agent and
// starts it again, exactly as a reboot's revive would (F1.2).
func TestReviveRun_AfterAnExtension(t *testing.T) {
	f := newReviveFixture(t) // already lost to an outage
	end := f.now.Add(time.Hour)
	f.st.mu.Lock()
	f.st.run.EndsAt = &end
	f.st.mu.Unlock()

	// The end passes while the run is still lost: the lease sweep stops the
	// agent, but the label stays outage.
	f.now = end.Add(time.Minute)
	f.sweep(t)
	if f.rn.endCount() != 1 {
		t.Fatalf("setup: EndSandbox calls = %d, want 1 (an outage run's agent stops once its end passes)", f.rn.endCount())
	}
	if _, reason := f.st.lost(); reason != types.LostOutage {
		t.Fatalf("setup: lost reason = %q, want it to stay outage even though the agent stopped", reason)
	}

	sr := &startingRunner{reviveRunner: f.rr}
	sr.status = types.RunStopped // what a real driver's Status would now report
	f.srv.cfg.Runner = sr

	later := f.now.Add(24 * time.Hour)
	if w := do(t, f.srv, http.MethodPatch, "/api/v1/runs/"+f.run.ID.String(), adminToken, endsAtBody(later)); w.Code != http.StatusOK {
		t.Fatalf("extend past its end: code %d body %s, want 200", w.Code, w.Body.String())
	}

	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive after the extension: code %d, want 200", code)
	}
	if got := sr.starts(); !slices.Equal(got, []int{1}) {
		t.Fatalf("StartSandbox calls, by proxies replaced before each = %v; want one start, after the new proxy", got)
	}
	if lostAt, _ := f.st.lost(); lostAt != nil || f.st.State() != types.RunRunning {
		t.Errorf("lost %v, state %s; want the run live and RUNNING", lostAt, f.st.State())
	}
	ev := f.audit.eventsFor(f.run.ID, "run.revive")
	if len(ev) != 1 || ev[0].Outcome != "success" {
		t.Fatalf("run.revive events = %+v, want one success", ev)
	}
	if data := leaseAuditData(t, ev[0]); data["from"] != "outage" || data["agent_started"] != true {
		t.Errorf("run.revive data = %v; want from outage, agent_started (the agent was stopped, so revive started it)", data)
	}
}
