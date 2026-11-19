// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestResolveUpstreamProxyURL covers the site-config → ProxyConfig.UpstreamProxyURL
// resolution dispatchRun performs: a plain URL is preferred when set, a
// secret ref resolves to a URL otherwise, neither configured is a safe no-op,
// and an https value (from either source) is skipped (the sidecar's
// parseUpstreamProxy only supports http — see resolveUpstreamProxyURL's doc).
func TestResolveUpstreamProxyURL(t *testing.T) {
	ctx := context.Background()
	sec := &memSecrets{m: map[string][]byte{
		"corp-proxy-url":       []byte("http://user:pass@proxy.corp:8080"),
		"corp-proxy-url-https": []byte("https://proxy.corp:8443"),
	}}

	t.Run("ref resolves to URL", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "", "corp-proxy-url", sec.Get)
		if reason != "" {
			t.Fatalf("failReason = %q, want \"\"", reason)
		}
		if url != "http://user:pass@proxy.corp:8080" {
			t.Errorf("url = %q, want the stored value verbatim", url)
		}
	})

	t.Run("neither configured is a no-op, not a crash", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "", "", sec.Get)
		if url != "" || reason != "" {
			t.Errorf("empty plainURL+ref: got (%q, %q), want (\"\", \"\")", url, reason)
		}
	})

	t.Run("missing secret fails safe", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "", "no-such-secret", sec.Get)
		if url != "" {
			t.Errorf("url = %q, want empty on unresolved secret", url)
		}
		if reason != "secret-not-found" {
			t.Errorf("reason = %q, want secret-not-found", reason)
		}
	})

	t.Run("no secret store configured fails safe", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "", "corp-proxy-url", nil)
		if url != "" || reason != "no-secret-store" {
			t.Errorf("got (%q, %q), want (\"\", \"no-secret-store\")", url, reason)
		}
	})

	t.Run("https ref is skipped (sidecar only supports http)", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "", "corp-proxy-url-https", sec.Get)
		if url != "" {
			t.Errorf("url = %q, want empty for an https upstream proxy", url)
		}
		if reason != "unsupported-scheme" {
			t.Errorf("reason = %q, want unsupported-scheme", reason)
		}
	})

	t.Run("reserved secret name rejected defense-in-depth", func(t *testing.T) {
		reservedSec := &memSecrets{m: map[string][]byte{"wardyn-signing-key": []byte("http://sneaky:8080")}}
		url, reason := resolveUpstreamProxyURL(ctx, "", "wardyn-signing-key", reservedSec.Get)
		if url != "" || reason != "reserved-secret-name" {
			t.Errorf("got (%q, %q), want (\"\", \"reserved-secret-name\")", url, reason)
		}
	})

	t.Run("plain URL is preferred when both are set", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "http://proxy.corp:9999", "corp-proxy-url", sec.Get)
		if reason != "" {
			t.Fatalf("failReason = %q, want \"\"", reason)
		}
		if url != "http://proxy.corp:9999" {
			t.Errorf("url = %q, want the plain URL to win over the secret ref", url)
		}
	})

	t.Run("plain URL alone resolves, no secret store needed", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "http://proxy.corp:3128", "", nil)
		if reason != "" {
			t.Fatalf("failReason = %q, want \"\"", reason)
		}
		if url != "http://proxy.corp:3128" {
			t.Errorf("url = %q, want http://proxy.corp:3128", url)
		}
	})

	t.Run("plain https URL is skipped, no fallback to the secret ref", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "https://proxy.corp:8443", "corp-proxy-url", sec.Get)
		if url != "" || reason != "unsupported-scheme" {
			t.Errorf("got (%q, %q), want (\"\", \"unsupported-scheme\") — a bad plain URL must fail safe, "+
				"not silently fall back to the secret", url, reason)
		}
	})
}

// TestUpstreamProxy_MemberRowNeverChangesURL is the negative control for
// resolveRunUpstreamProxy's deliberate operator-only scoping (0.7, migration
// 0050): the upstream proxy's own address is never run through VetHost/the
// private-IP guard (proxy.go) — it is CONNECTed to directly as the operator's
// trusted corp-network hop — so a member-substitutable upstream would let a
// member redirect every run's egress to a server of their own choosing with
// no SSRF guard on that hop at all. A member row sharing the site-config
// secret ref's name must never change the resolved URL — resolveRunUpstreamProxy
// always reads For("").
func TestUpstreamProxy_MemberRowNeverChangesURL(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{"corp-proxy-url": []byte("http://legit-proxy.corp:8080")}}
	if err := sec.For("alice").Put(context.Background(), "corp-proxy-url", []byte("http://attacker.evil:8080")); err != nil {
		t.Fatalf("seed alice's row: %v", err)
	}
	h := newHarness(t)
	h.srv.cfg.Secrets = sec
	siteCfg := types.SiteConfig{UpstreamProxySecretRef: "corp-proxy-url"}

	got := h.srv.resolveRunUpstreamProxy(context.Background(), uuid.New(), siteCfg, nil, "")
	if got != "http://legit-proxy.corp:8080" {
		t.Fatalf("resolveRunUpstreamProxy = %q, want the operator's URL — "+
			"a member row named as the site-config ref must never change it", got)
	}
}

// TestUpstreamProxy_SecretLaneResolvesThroughTheAuditedStore: wardynd's store
// is secretstore.Audited, which refuses a read whose context says no purpose.
// The secret lane must mark its read, or every run silently falls back to
// direct egress, recorded as secret-not-found.
func TestUpstreamProxy_SecretLaneResolvesThroughTheAuditedStore(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{"corp-proxy-url": []byte("http://legit-proxy.corp:8080")}}
	h := newHarness(t)
	h.srv.cfg.Secrets = secretstore.Audited(sec, &recRecorder{})
	siteCfg := types.SiteConfig{UpstreamProxySecretRef: "corp-proxy-url"}
	if got := h.srv.resolveRunUpstreamProxy(context.Background(), uuid.New(), siteCfg, nil, ""); got != "http://legit-proxy.corp:8080" {
		t.Fatalf("resolveRunUpstreamProxy through the audited store = %q, want the secret's URL", got)
	}
}

// TestUpstreamProxyURL_PortIsGatedByTheSidecarsOwnLoader pins the write-time and
// dispatch-time halves of one rule: an upstream proxy URL the sidecar's own
// loader refuses must never be persisted, and must never be delivered to a
// sidecar if it is already stored. The API package's own gate checked only
// url.Parse + the http scheme (hostrules.HostOf discards the port entirely), so
// ":0" and ":99999" saved with 200 OK and then failed
// Config.applyDefaultsAndValidate at container start — cmd/wardyn-proxy
// os.Exit(1)s on that, killing the egress sidecar of EVERY dispatched run. The
// gate is proxy.ValidUpstreamProxyURL, the same delegation upstream_proxy_no_proxy
// already makes to proxy.ValidNoProxyEntry, so one matcher decides.
func TestUpstreamProxyURL_PortIsGatedByTheSidecarsOwnLoader(t *testing.T) {
	bad := []string{"http://proxy.corp.internal:0", "http://proxy.corp.internal:99999"}
	for _, raw := range bad {
		t.Run("PUT refuses "+raw, func(t *testing.T) {
			// The write-time gate, on the same value the sidecar would load.
			if err := validateSiteConfig(types.SiteConfig{UpstreamProxyURL: raw}); err == nil {
				t.Fatalf("validateSiteConfig(%q) = nil: the sidecar's own loader (proxy.ValidUpstreamProxyURL over "+
					"parseUpstreamProxy) refuses this port, so PUT /site-config must not persist it", raw)
			}
		})
		t.Run("dispatch drops "+raw, func(t *testing.T) {
			// The plain lane, for a row written before the gate existed.
			if url, reason := resolveUpstreamProxyURL(context.Background(), raw, "", nil); url != "" || reason == "" {
				t.Errorf("plain lane: got (%q, %q), want (\"\", a fail reason) — delivering this URL "+
					"os.Exit(1)s the sidecar at container start", url, reason)
			}
			// The SECRET lane, which no write-time validator can see inside.
			sec := &memSecrets{m: map[string][]byte{"corp-proxy-url": []byte(raw)}}
			if url, reason := resolveUpstreamProxyURL(context.Background(), "", "corp-proxy-url", sec.Get); url != "" || reason == "" {
				t.Errorf("secret lane: got (%q, %q), want (\"\", a fail reason) — a credentialed URL in a "+
					"secret must not smuggle a port the sidecar refuses", url, reason)
			}
		})
	}
	// The control: a good URL still resolves, from both lanes, and still saves.
	if err := validateSiteConfig(types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.internal:3128"}); err != nil {
		t.Fatalf("a valid upstream proxy URL must still save: %v", err)
	}
	sec := &memSecrets{m: map[string][]byte{"corp-proxy-url": []byte("http://user:pass@proxy.corp:8080")}}
	if url, reason := resolveUpstreamProxyURL(context.Background(), "", "corp-proxy-url", sec.Get); reason != "" || url == "" {
		t.Fatalf("a valid credentialed secret URL must still resolve, got (%q, %q)", url, reason)
	}
}

// TestResolveRunUpstreamProxy_NotConfiguredIsAudited: a run with no upstream proxy
// configured still records run.upstream_proxy.resolve, so an operator can tell
// "no upstream configured" from "the event is missing".
func TestResolveRunUpstreamProxy_NotConfiguredIsAudited(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	if got := h.srv.resolveRunUpstreamProxy(context.Background(), runID, types.SiteConfig{}, nil, ""); got != "" {
		t.Fatalf("resolveRunUpstreamProxy = %q, want \"\" (direct egress)", got)
	}
	ev := findAudit(h.audit.events, runID, "run.upstream_proxy.resolve", "success")
	if ev == nil {
		t.Fatalf("no run.upstream_proxy.resolve success event; events=%s", auditDump(h.audit.events, runID))
	}
	var data struct {
		InEffect *bool  `json:"in_effect"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("event data is not an object: %v (%s)", err, ev.Data)
	}
	if data.InEffect == nil || *data.InEffect || data.Reason != "not-configured" {
		t.Errorf("event data = %s, want in_effect false and reason not-configured", ev.Data)
	}
}

// TestResolveRunUpstreamProxy_NoHopReasons: a run with a reason to have no upstream proxy hop (No network, or a
// person's runner, which is never sent the organisation's upstream) gets none even when the site configures one,
// and its run.upstream_proxy.resolve row says so (in_effect false, the reason), so its audit never records a hop
// the run does not have. The same site config with no reason still resolves the hop.
func TestResolveRunUpstreamProxy_NoHopReasons(t *testing.T) {
	siteCfg := types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.internal:3128"}
	for _, reason := range []string{upstreamNoHopNoNetwork, upstreamNoHopNotSent} {
		t.Run(reason, func(t *testing.T) {
			h := newHarness(t)
			runID := uuid.New()
			if got := h.srv.resolveRunUpstreamProxy(context.Background(), runID, siteCfg, nil, reason); got != "" {
				t.Fatalf("resolveRunUpstreamProxy = %q, want \"\"", got)
			}
			ev := findAudit(h.audit.snapshot(), runID, "run.upstream_proxy.resolve", "success")
			if ev == nil {
				t.Fatalf("no run.upstream_proxy.resolve success event; events=%s", auditDump(h.audit.snapshot(), runID))
			}
			if string(ev.Data) != `{"in_effect":false,"reason":"`+reason+`"}` {
				t.Errorf("event data = %s, want in_effect false and reason %s", ev.Data, reason)
			}
			// The reason is checked before the site config read error: such a run has no hop either way.
			errID := uuid.New()
			if got := h.srv.resolveRunUpstreamProxy(context.Background(), errID, siteCfg, errors.New("site config unreadable"), reason); got != "" {
				t.Fatalf("resolveRunUpstreamProxy with a site config read error = %q, want \"\"", got)
			}
			if ev := findAudit(h.audit.snapshot(), errID, "run.upstream_proxy.resolve", "success"); ev == nil || string(ev.Data) != `{"in_effect":false,"reason":"`+reason+`"}` {
				t.Errorf("with a site config read error: event = %v, want success, in_effect false and reason %s; events=%s", ev, reason, auditDump(h.audit.snapshot(), errID))
			}
		})
	}
	h := newHarness(t)
	if got := h.srv.resolveRunUpstreamProxy(context.Background(), uuid.New(), siteCfg, nil, ""); got != "http://proxy.corp.internal:3128" {
		t.Errorf("resolveRunUpstreamProxy with no reason = %q, want the site's upstream proxy", got)
	}
}

// TestUpstreamNoHopReason: No network wins over a person's runner, a person's runner has no hop, and a run on the
// organisation's own executor (remote, or stored before placements) keeps the site's hop.
func TestUpstreamNoHopReason(t *testing.T) {
	nn := &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Scope: types.WallPresetScopeBuiltin}
	for _, tc := range []struct {
		name string
		run  types.AgentRun
		want string
	}{
		{"remote", types.AgentRun{Placement: types.PlacementRemote}, ""},
		{"stored before placements", types.AgentRun{}, ""},
		{"a person's runner", types.AgentRun{Placement: types.PlacementLocal}, upstreamNoHopNotSent},
		{"No network on a person's runner", types.AgentRun{Placement: types.PlacementLocal, WallPreset: nn}, upstreamNoHopNoNetwork},
		{"No network, remote", types.AgentRun{Placement: types.PlacementRemote, WallPreset: nn}, upstreamNoHopNoNetwork},
	} {
		if got := upstreamNoHopReason(tc.run); got != tc.want {
			t.Errorf("%s: upstreamNoHopReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestDispatchLocalRunHasNoUpstreamHop: dispatching a run placed on a person's runner records
// run.upstream_proxy.resolve as in_effect false, reason not-sent-to-runner, though the site configures an
// upstream proxy: the organisation's upstream is never sent to a person's runner.
func TestDispatchLocalRunHasNoUpstreamHop(t *testing.T) {
	srv, ls, rec, run := localDispatchFixture(t, types.PlacementLocal)
	runnerID := uuid.New()
	run.RunnerID = &runnerID
	ls.dispatchTestStore.run, ls.dispatchTestStore.state = run, types.RunPending
	sc := types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.internal:3128"}
	srv.cfg.Store = siteConfigDispatchStore{localDispatchStore: ls, sc: &sc}
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "busybox:latest", Interactive: true,
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
	})
	ev := findAudit(rec.snapshot(), run.ID, "run.upstream_proxy.resolve", "success")
	if ev == nil {
		t.Fatalf("no run.upstream_proxy.resolve success event; events=%s", auditDump(rec.snapshot(), run.ID))
	}
	if string(ev.Data) != `{"in_effect":false,"reason":"not-sent-to-runner"}` {
		t.Errorf("event data = %s, want in_effect false and reason not-sent-to-runner", ev.Data)
	}
}

// TestLocalReviveHasNoUpstreamHop: reviving a run on a person's runner leaves the organisation's upstream proxy out
// of its config though the site configures one, and records run.upstream_proxy.resolve with reason
// not-sent-to-runner, as its dispatch does.
func TestLocalReviveHasNoUpstreamHop(t *testing.T) {
	f := newComponentFixture(t)
	runnerID := uuid.New()
	run := types.AgentRun{ID: uuid.New(), CreatedBy: capSub, Placement: types.PlacementLocal, RunnerID: &runnerID}
	f.srv.cfg.Store = residentFactsStore{localPlanTestStore: localPlanTestStore{f.st},
		policy:  json.RawMessage(`{"classes":{},"by_trust":{"org_cluster":{"classes":{"env_secret":{"mode":"runner_resident"}}}}}`),
		posture: types.RunnerPosture{OS: "linux", Evidence: types.PostureEvidenceOSProbe},
		trust:   types.RunnerTrustOrgCluster, recorded: types.RunnerTrustOrgCluster}
	cfg := proxy.Config{RunID: run.ID}
	sc := types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.internal:3128"}
	if err := f.srv.refreshLocalDeploymentConfig(context.Background(), run, sc, &cfg); err != nil {
		t.Fatalf("revive: %v", err)
	}
	if cfg.UpstreamProxyURL != "" {
		t.Errorf("revived upstream proxy = %q, want none on a person's runner", cfg.UpstreamProxyURL)
	}
	ev := findAudit(f.rec.snapshot(), run.ID, "run.upstream_proxy.resolve", "success")
	if ev == nil || string(ev.Data) != `{"in_effect":false,"reason":"not-sent-to-runner"}` {
		t.Errorf("run.upstream_proxy.resolve success event = %v, want in_effect false and reason not-sent-to-runner; events=%s", ev, auditDump(f.rec.snapshot(), run.ID))
	}
}

// TestRemoteReviveUpstreamHop: a revive on the organisation's own executor refreshes the site's upstream proxy into
// the run's config, except for a No network run, which gets none and is recorded with reason no-network.
func TestRemoteReviveUpstreamHop(t *testing.T) {
	nn := &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Scope: types.WallPresetScopeBuiltin}
	for _, tc := range []struct {
		name   string
		preset *types.WallPresetOnRun
		want   string
		data   string
	}{
		{"the site's hop", nil, "http://proxy.corp.internal:3128", `"in_effect":true`},
		{"No network", nn, "", `{"in_effect":false,"reason":"no-network"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ls, rec, run := localDispatchFixture(t, types.PlacementRemote)
			run.WallPreset = tc.preset
			sc := types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.internal:3128"}
			srv.cfg.Store = siteConfigDispatchStore{localDispatchStore: ls, sc: &sc}
			cfg := &proxy.Config{RunID: run.ID}
			if err := srv.refreshDeploymentConfig(context.Background(), run, cfg); err != nil {
				t.Fatalf("revive: %v", err)
			}
			if cfg.UpstreamProxyURL != tc.want {
				t.Errorf("revived upstream proxy = %q, want %q", cfg.UpstreamProxyURL, tc.want)
			}
			ev := findAudit(rec.snapshot(), run.ID, "run.upstream_proxy.resolve", "success")
			if ev == nil || !strings.Contains(string(ev.Data), tc.data) {
				t.Errorf("run.upstream_proxy.resolve success event = %v, want data holding %s; events=%s", ev, tc.data, auditDump(rec.snapshot(), run.ID))
			}
		})
	}
}

// TestViaOrgEngineUpstreamHop: the organisation-side engine of a run on a person's runner chains through the
// organisation's upstream proxy, so it records run.upstream_proxy.resolve with in_effect true under that run,
// while a No network run's engine has no hop and records reason no-network.
func TestViaOrgEngineUpstreamHop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		noNetwork bool
		want      string
		data      string
	}{
		{"the organisation's hop", false, "http://proxy.corp.internal:3128", `"in_effect":true`},
		{"No network", true, "", `{"in_effect":false,"reason":"no-network"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ls, rec, run := localDispatchFixture(t, types.PlacementLocal)
			ctx := context.Background()
			spec := localOrgNetworkSpec(run.ID)
			spec.ProxyConfig.ControlPlaneURL = "http://127.0.0.1:9"
			if !srv.classifyLocalDispatch(ctx, run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), types.SiteConfig{}, &spec, nil, llmTransport{}, false) {
				t.Fatal("an all-own plan was refused")
			}
			if tc.noNetwork {
				raw, err := srv.loadRunProxyConfig(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				var stored map[string]any
				if err := json.Unmarshal(raw, &stored); err != nil {
					t.Fatal(err)
				}
				stored["no_network"] = true
				if err := srv.storeRunProxyConfig(ctx, run.ID, mustJSON(stored)); err != nil {
					t.Fatal(err)
				}
			}
			sc := types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.internal:3128"}
			srv.cfg.Store = siteConfigDispatchStore{localDispatchStore: ls, sc: &sc}
			cfg, err := srv.viaOrgEngineConfig(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.UpstreamProxyURL != tc.want {
				t.Errorf("the organisation-side engine's upstream proxy = %q, want %q", cfg.UpstreamProxyURL, tc.want)
			}
			ev := findAudit(rec.snapshot(), run.ID, "run.upstream_proxy.resolve", "success")
			if ev == nil || !strings.Contains(string(ev.Data), tc.data) {
				t.Errorf("run.upstream_proxy.resolve success event = %v, want data holding %s; events=%s", ev, tc.data, auditDump(rec.snapshot(), run.ID))
			}
		})
	}
}
