// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	viaOrgADOHost = "dev.azure.com"
	viaOrgADOInfo = "/wardyn/git/dev.azure.com/acme/proj/_git/app/info/refs?service=git-upload-pack"
)

// adoEngineConfig adds the run's Azure DevOps grant and its token injection to an engine config: the
// shape the organisation builds for a via_org Azure DevOps grant.
func adoEngineConfig(caps ...adoscope.Capability) func(*Config) {
	return func(c *Config) {
		c.Policy.AllowedDomains = append(c.Policy.AllowedDomains, viaOrgADOHost)
		c.Injection = append(c.Injection, InjectionConfig{GrantID: uuid.New(),
			InjectionRule: egress.InjectionRule{Host: viaOrgADOHost, Header: "Authorization", SecretName: types.ADOEntraAccessTokenSecret}})
		c.MITMHosts = append(c.MITMHosts, viaOrgADOHost)
		c.ADOGrant = &ADOGrantConfig{Organization: "acme", Capabilities: caps, Hosts: []string{viaOrgADOHost}}
	}
}

// A runner's proxy with an Azure DevOps via_org route and no grant of its own forwards /wardyn/git/<host>/ to the
// organisation on that route: it has no Azure DevOps credential to broker with.
func TestLaptopProxy_ViaOrgForwardsADOGit(t *testing.T) {
	f := newLaptopFixture(t)
	grant := uuid.New()
	f.p.viaOrgRoutes = append(f.p.viaOrgRoutes, ViaOrgRoute{GrantID: grant, Host: viaOrgADOHost})
	w := httptest.NewRecorder()
	f.p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, viaOrgADOInfo, nil))
	if w.Body.String() != "from-org" || f.cpSeen != 1 || f.lHits != 0 {
		t.Fatalf("answer %q, org saw %d, laptop dialed %d, want the organisation's answer once", w.Body, f.cpSeen, f.lHits)
	}
	if want := ViaOrgPathPrefix + grant.String() + "/wardyn/git/dev.azure.com/acme/proj/_git/app/info/refs"; f.cpReq.URL.Path != want {
		t.Fatalf("forwarded to %s, want %s", f.cpReq.URL.Path, want)
	}
}

// A proxy that holds the Azure DevOps grant itself (the resident shape) serves the request itself: the route
// is the organisation's only for a host this proxy cannot broker.
func TestLaptopProxy_ViaOrgADOGitStaysLocalWithTheGrant(t *testing.T) {
	f := newLaptopFixture(t)
	f.p.viaOrgRoutes = append(f.p.viaOrgRoutes, ViaOrgRoute{GrantID: uuid.New(), Host: viaOrgADOHost})
	f.p.adoGrants = adoGrantsByHost{viaOrgADOHost: ADOGrant{Organization: "acme", Capabilities: []adoscope.Capability{adoscope.CapCodeRead}}}
	w := httptest.NewRecorder()
	f.p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, viaOrgADOInfo, nil))
	if f.cpSeen != 0 || w.Body.String() == "from-org" {
		t.Fatalf("the organisation saw %d requests (%q), want 0: this proxy brokers the host itself", f.cpSeen, w.Body)
	}
}

// The engine serves an Azure DevOps upload-pack with the run's grant, and refuses a push the grant's
// capabilities withhold in the broker's own sentence, before the upstream sees it.
func TestViaOrgEngine_ServesADOGitWithinTheGrant(t *testing.T) {
	f := newViaOrgFixture(t, nil, adoEngineConfig(adoscope.CapCodeRead))
	w := httptest.NewRecorder()
	f.engine.ServeBroker(w, mustLocalReq(t, http.MethodPost, "/wardyn/git/dev.azure.com/acme/proj/_git/app/git-upload-pack", strings.NewReader("0000")))
	if w.Code != http.StatusOK || w.Body.String() != "upstream-ok" {
		t.Fatalf("upload-pack: %d %q, want served", w.Code, w.Body)
	}
	if hits, h, path := f.up.seen(); hits != 1 || h.Get("Authorization") != viaOrgSecret || !strings.HasSuffix(path, "/git-upload-pack") {
		t.Fatalf("upstream hits=%d auth=%q path=%q, want one upload-pack carrying the org's credential", hits, h.Get("Authorization"), path)
	}

	run := BranchNSPrefix(f.runID)
	push := pkt(someOID+" "+otherOID+" "+run+"a"+firstCaps) + "0000"
	w = httptest.NewRecorder()
	f.engine.ServeBroker(w, mustLocalReq(t, http.MethodPost, "/wardyn/git/dev.azure.com/acme/proj/_git/app/git-receive-pack", strings.NewReader(push)))
	if body := w.Body.String(); !strings.Contains(body, string(adoscope.CapCodeWrite)) || !strings.Contains(body, "this run was not granted it") {
		t.Fatalf("push: %d %q, want the broker's refusal naming %s", w.Code, body, adoscope.CapCodeWrite)
	}
	if hits, _, _ := f.up.seen(); hits != 1 {
		t.Fatalf("the refused push reached the upstream: %d hits, want 1", hits)
	}
}

// A /wardyn/git/ host outside the engine's Azure DevOps grant is refused 403 and never dialed, as is a
// host the grant covers when the engine holds no grant at all.
func TestViaOrgEngine_ADOGitOutsideTheGrantIs403(t *testing.T) {
	f := newViaOrgFixture(t, nil, adoEngineConfig(adoscope.CapCodeRead))
	for _, path := range []string{
		"/wardyn/git/example.com/acme/proj/_git/app/info/refs?service=git-upload-pack",
		"/wardyn/git/acme.visualstudio.com/proj/_git/app/info/refs?service=git-upload-pack",
	} {
		w := httptest.NewRecorder()
		f.engine.ServeBroker(w, mustLocalReq(t, http.MethodGet, path, nil))
		// The engine's own sentence: the broker's would also be a 403, but only after handing the request to it.
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not a brokered route") {
			t.Errorf("%s: %d %q, want 403 not a brokered route", path, w.Code, w.Body)
		}
	}
	bare := newViaOrgFixture(t, nil, nil)
	w := httptest.NewRecorder()
	bare.engine.ServeBroker(w, mustLocalReq(t, http.MethodGet, viaOrgADOInfo, nil))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not a brokered route") {
		t.Errorf("an engine with no Azure DevOps grant answered %d, want 403", w.Code)
	}
	if hits, _, _ := f.up.seen(); hits != 0 {
		t.Fatalf("a refused host was dialed %d times", hits)
	}
}

// No engine is built with an Azure DevOps token and no grant to gate it: the token bounds nothing on its own.
func TestNewViaOrgEngine_RefusesAnUngatedADOToken(t *testing.T) {
	cfg := &Config{RunID: uuid.New(), ControlPlaneURL: "https://wardynd.test", RunToken: "RUNTOK",
		Policy: types.RunPolicySpec{AllowedDomains: []string{viaOrgADOHost}},
		Injection: []InjectionConfig{{GrantID: uuid.New(),
			InjectionRule: egress.InjectionRule{Host: viaOrgADOHost, Header: "Authorization", SecretName: types.ADOEntraAccessTokenSecret}}},
		MITMHosts: []string{viaOrgADOHost}}
	e, err := NewViaOrgEngine(t.Context(), cfg, ViaOrgEngineOptions{Client: &http.Client{Transport: &fakeOrg{}}})
	if !errors.Is(err, ErrViaOrgADOUngated) || e != nil {
		t.Fatalf("engine %v err %v, want ErrViaOrgADOUngated", e, err)
	}
	// A grant that covers another host is no cover for this one.
	cfg.ADOGrant = &ADOGrantConfig{Organization: "acme", Hosts: []string{"acme.visualstudio.com"}}
	if _, err := NewViaOrgEngine(t.Context(), cfg, ViaOrgEngineOptions{Client: &http.Client{Transport: &fakeOrg{}}}); !errors.Is(err, ErrViaOrgADOUngated) {
		t.Fatalf("err %v, want ErrViaOrgADOUngated for a host the grant does not cover", err)
	}
}
