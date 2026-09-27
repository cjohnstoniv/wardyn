// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// Runs dispatched before the proxy's control-plane hop moved to TLS (#994)
// kept a rendered proxy config naming http://wardynd:8080 and no hop CA. The
// strict loader refuses that URL now, so revive, restart with current limits
// and extension refused exactly the standing runs they exist for.

// legacyHop turns the fixture's rendered config into the pre-TLS shape and
// applies edit (another field, another run) on the raw object, as a config
// read back from an older proxy would arrive.
func (f *reviveFixture) legacyHop(t *testing.T, edit func(map[string]json.RawMessage)) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(f.rr.cfg, &m); err != nil {
		t.Fatal(err)
	}
	m["control_plane_url"] = json.RawMessage(`"http://wardynd:8080"`)
	delete(m, "control_plane_ca_pem")
	if edit != nil {
		edit(m)
	}
	var err error
	if f.rr.cfg, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.LoadConfigBytes(f.rr.cfg); err == nil {
		t.Fatal("fixture: the pre-TLS config loads as rendered; the case would prove nothing")
	}
}

// restart POSTs the admin bulk restart for the fixture's run and returns its
// one result.
func (f *reviveFixture) restart(t *testing.T) adminRestartResult {
	t.Helper()
	w := do(t, f.srv, http.MethodPost, "/api/v1/admin/runs/restart", adminToken, `{"run_ids":["`+f.run.ID.String()+`"]}`)
	var out struct {
		Results []adminRestartResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK || len(out.Results) != 1 {
		t.Fatalf("restart = %d %s (%v), want 200 with one result", w.Code, w.Body.String(), err)
	}
	return out.Results[0]
}

// legacyDoors are the three ways a standing run gets a new proxy: its owner's
// revive, an admin's revive, and the admin restart of a live run.
var legacyDoors = []struct {
	name string
	live bool
	call func(t *testing.T, f *reviveFixture) (ok bool, detail string)
}{
	{"owner revive", false, func(t *testing.T, f *reviveFixture) (bool, string) {
		code, body := f.reviveAs(t, true)
		return code == http.StatusOK, body
	}},
	{"admin revive", false, func(t *testing.T, f *reviveFixture) (bool, string) {
		code, body := f.reviveAs(t, false)
		return code == http.StatusOK, body
	}},
	{"admin restart", true, func(t *testing.T, f *reviveFixture) (bool, string) {
		r := f.restart(t)
		return r.OK, r.Error
	}},
}

func newLegacyFixture(t *testing.T, live bool) *reviveFixture {
	t.Helper()
	f, _ := newOwnerFixture(t)
	if live {
		f.st.run.LostAt, f.st.run.LostReason = nil, ""
	}
	f.legacyHop(t, nil)
	return f
}

// TestReviveRun_PreTLSConfigUsesCurrentTLSHop: every door gives a pre-TLS run
// a proxy that dials the deployment's CURRENT TLS hop under its current CA,
// with a fresh token.
func TestReviveRun_PreTLSConfigUsesCurrentTLSHop(t *testing.T) {
	for _, door := range legacyDoors {
		t.Run(door.name, func(t *testing.T) {
			f := newLegacyFixture(t, door.live)
			if ok, detail := door.call(t, f); !ok {
				t.Fatalf("%s of a pre-TLS run refused: %s", door.name, detail)
			}
			cfg := f.newConfig(t)
			if cfg.ControlPlaneURL != "https://wardynd:8443" || cfg.ControlPlaneCAPEM != f.currentCAPEM {
				t.Errorf("control plane %q, CA current=%v; want the deployment's current TLS hop and its CA",
					cfg.ControlPlaneURL, cfg.ControlPlaneCAPEM == f.currentCAPEM)
			}
			if cfg.RunToken == "old-token" || cfg.RunToken == "" {
				t.Errorf("run token %q; want a fresh one", cfg.RunToken)
			}
			if lostAt, _ := f.st.lost(); lostAt != nil {
				t.Error("the run is still lost after its proxy was replaced")
			}
		})
	}
}

// TestReviveRun_PreTLSConfigKeepsMITMCAAndPolicy: the hop rewrite touches
// nothing else. The run keeps its own MITM CA and allowlist, and the owner's
// current denies still land with their credential lanes dropped.
func TestReviveRun_PreTLSConfigKeepsMITMCAAndPolicy(t *testing.T) {
	f := newLegacyFixture(t, false)
	if code, body := f.reviveAs(t, false); code != http.StatusOK {
		t.Fatalf("revive = %d %s, want 200", code, body)
	}
	cfg := f.newConfig(t)
	if cfg.RunID != f.run.ID || cfg.MITMCACertPEM != "ca-cert" || cfg.MITMCAKeyPEM != "ca-key" {
		t.Errorf("run %s, MITM CA %q/%q; want this run and its own MITM CA carried over", cfg.RunID, cfg.MITMCACertPEM, cfg.MITMCAKeyPEM)
	}
	if !slices.Equal(cfg.Policy.AllowedDomains, []string{"api.openai.com", "api.anthropic.com"}) {
		t.Errorf("allowed_domains = %v; want the frozen allowlist", cfg.Policy.AllowedDomains)
	}
	for _, host := range f.profile.Ceiling.DeniedDomains {
		if !slices.Contains(cfg.Policy.DeniedDomains, host) {
			t.Errorf("denied_domains = %v; want the owner's deny %q", cfg.Policy.DeniedDomains, host)
		}
	}
	if slices.ContainsFunc(cfg.Injection, func(in proxy.InjectionConfig) bool { return in.Host == "api.openai.com" }) {
		t.Error("the injection to a host the owner's profile denies survived")
	}
	if _, ok := cfg.PATGrants["pat.example"]; ok {
		t.Error("the brokered PAT for a host the owner's profile denies survived")
	}
}

// TestReviveRun_RewrittenConfigStillRejectsUnknownFields: the rewrite is on
// the raw object, so a field the loader does not know still refuses the run,
// through every door.
func TestReviveRun_RewrittenConfigStillRejectsUnknownFields(t *testing.T) {
	for _, door := range legacyDoors {
		t.Run(door.name, func(t *testing.T) {
			f, _ := newOwnerFixture(t)
			if door.live {
				f.st.run.LostAt, f.st.run.LostReason = nil, ""
			}
			f.legacyHop(t, func(m map[string]json.RawMessage) { m["surprise_lane"] = json.RawMessage(`true`) })
			ok, detail := door.call(t, f)
			if ok || !strings.Contains(detail, "does not load") || !strings.Contains(detail, "surprise_lane") {
				t.Fatalf("%s = ok %v %q; want refused naming the unknown field", door.name, ok, detail)
			}
			if len(f.rr.replaced) != 0 {
				t.Error("a refused revive replaced the proxy")
			}
		})
	}
}

// mintCountingIdentity counts mints, to prove a refusal came before one.
type mintCountingIdentity struct {
	identity.Provider
	mints int
}

func (c *mintCountingIdentity) MintRunIdentity(ctx context.Context, runID uuid.UUID, sub, sponsor, aud string, operatorOwned bool) (identity.RunIdentity, error) {
	c.mints++
	return c.Provider.MintRunIdentity(ctx, runID, sub, sponsor, aud, operatorOwned)
}

// countingImages counts proxy image preparations.
type countingImages struct {
	*reviveRunner
	ensures int
}

func (c *countingImages) EnsureProxyImage(ctx context.Context) error {
	c.ensures++
	return c.reviveRunner.EnsureProxyImage(ctx)
}

// TestReviveRun_InvalidCurrentHopRefusesBeforeReplacement: the rewritten
// config is still validated, so a deployment whose OWN current hop the proxy
// would refuse (plain http to a non-loopback host) refuses the run before it
// mints a token, prepares an image, claims the run or touches its proxy.
func TestReviveRun_InvalidCurrentHopRefusesBeforeReplacement(t *testing.T) {
	for _, door := range legacyDoors {
		t.Run(door.name, func(t *testing.T) {
			f := newLegacyFixture(t, door.live)
			f.srv.cfg.ControlPlaneURL, f.srv.cfg.ControlPlaneCAPEM = "http://wardynd:8080", ""
			ids := &mintCountingIdentity{Provider: f.srv.cfg.Identity}
			imgs := &countingImages{reviveRunner: f.rr}
			f.srv.cfg.Identity, f.srv.cfg.Runner = ids, imgs
			lostBefore, _ := f.st.lost()

			ok, detail := door.call(t, f)
			if ok || !strings.Contains(detail, "does not load") {
				t.Fatalf("%s = ok %v %q; want refused: the current hop does not load", door.name, ok, detail)
			}
			if ids.mints != 0 || imgs.ensures != 0 || len(f.rr.replaced) != 0 {
				t.Errorf("mints %d, image preparations %d, replaces %d; want none before the refusal",
					ids.mints, imgs.ensures, len(f.rr.replaced))
			}
			if lostAfter, _ := f.st.lost(); (lostBefore == nil) != (lostAfter == nil) {
				t.Errorf("lost mark %v -> %v; want the run left unclaimed", lostBefore, lostAfter)
			}
		})
	}
}

// TestReviveRun_LegacyConfigNamesAnotherRunRefused: the run-id check still
// holds over a rewritten config.
func TestReviveRun_LegacyConfigNamesAnotherRunRefused(t *testing.T) {
	for _, door := range legacyDoors {
		t.Run(door.name, func(t *testing.T) {
			f, _ := newOwnerFixture(t)
			if door.live {
				f.st.run.LostAt, f.st.run.LostReason = nil, ""
			}
			f.legacyHop(t, func(m map[string]json.RawMessage) {
				m["run_id"] = json.RawMessage(`"` + uuid.NewString() + `"`)
			})
			if ok, detail := door.call(t, f); ok || !strings.Contains(detail, "names another run") {
				t.Fatalf("%s = ok %v %q; want refused: the config names another run", door.name, ok, detail)
			}
			if len(f.rr.replaced) != 0 {
				t.Error("a refused revive replaced the proxy")
			}
		})
	}
}

// TestExtendRun_PreTLSConfigRechecksOwner: extension re-checks the owner from
// the same rendered config, so a pre-TLS run can have its end moved too.
func TestExtendRun_PreTLSConfigRechecksOwner(t *testing.T) {
	f := newLegacyFixture(t, true)
	end := f.now.Add(time.Hour)
	f.st.run.EndsAt = &end
	body := `{"ends_at":"` + f.now.Add(2*time.Hour).Format(time.RFC3339) + `"}`
	w := do(t, f.srv, http.MethodPatch, "/api/v1/runs/"+f.run.ID.String(), adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("extend of a pre-TLS run = %d %s, want 200", w.Code, w.Body.String())
	}
	if got, _ := f.st.lost(); got != nil {
		t.Error("extension changed the lost mark")
	}
	if f.st.run.EndsAt == nil || !f.st.run.EndsAt.Equal(f.now.Add(2*time.Hour)) {
		t.Errorf("ends_at = %v, want the extended end", f.st.run.EndsAt)
	}
}
