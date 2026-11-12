// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerVersionEditStore answers the run's runner row with a fixed Version.
type runnerVersionEditStore struct {
	*readbackStore
	version string
}

func (s runnerVersionEditStore) GetRunner(context.Context, uuid.UUID) (types.Runner, error) {
	return types.Runner{Version: s.version}, nil
}

// A live egress edit is refused only on positive evidence that the runner or the proxy image is older
// than firstRenewGateRelease. A version that cannot be read (an empty Version, a digest-only image, a
// development tag) is not checked, and passes. A checkout build reports its release with a "+<commit>"
// suffix, which is dropped first.
func TestOlderThanFirstRenewGate_NeedsPositiveEvidence(t *testing.T) {
	rid := uuid.New()
	for _, c := range []struct {
		version string
		want    bool
	}{
		{"0.10.0-rc.18", true},
		{"0.10.0-rc.18+abc123", true},
		{"v0.10.0-rc.17", true},
		{"0.9.8", true},
		{"0.10.0-rc.19", false},
		{"0.10.0-rc.19+abc123", false},
		{"0.10.0-rc.19+abc123-dirty", false},
		{"0.10.0", false},
		{"", false},
		{"dev", false},
		{"sha256:abcd", false},
	} {
		srv := &Server{cfg: Config{Store: versionRunnerStore{version: c.version}}}
		if got := srv.olderThanFirstRenewGate(context.Background(), types.AgentRun{ID: uuid.New(), RunnerID: &rid}); got != c.want {
			t.Errorf("runner version %q: older = %v, want %v", c.version, got, c.want)
		}
	}
	for image, want := range map[string]bool{
		"ghcr.io/example/wardyn-proxy:0.10.0-rc.18":           true,
		"ghcr.io/example/wardyn-proxy:0.10.0-rc.19":           false,
		"ghcr.io/example/wardyn-proxy:0.10.0-rc.19+abc123":    false,
		"ghcr.io/example/wardyn-proxy@sha256:" + hashHex(nil): false,
		"ghcr.io/example/wardyn-proxy:latest":                 false,
		"":                                                    false,
	} {
		srv := &Server{cfg: Config{ProxyImage: image}}
		if got := srv.olderThanFirstRenewGate(context.Background(), types.AgentRun{ID: uuid.New()}); got != want {
			t.Errorf("in-process proxy image %q: older = %v, want %v", image, got, want)
		}
	}
}

// TestEgressEdit_RefusesARunnerOrImageOlderThanTheGateRelease: an rc.18 runner Version, an rc.18
// checkout build, or an rc.18 wardynd proxy image tag gets 409 egress_edit_legacy_proxy before the
// claim: no replace, no stop, no lost mark. An rc.19 runner, an rc.19 checkout build, an empty Version
// and a digest-only image get past the support check.
func TestEgressEdit_RefusesARunnerOrImageOlderThanTheGateRelease(t *testing.T) {
	refuse := func(t *testing.T, f *readbackFixture) {
		t.Helper()
		stops := f.lr.proxyStopCount()
		w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
		if w.Code != http.StatusConflict || errorReason(w) != reasonEgressEditLegacyProxy {
			t.Fatalf("edit: %d %s, want 409 %s", w.Code, w.Body.String(), reasonEgressEditLegacyProxy)
		}
		if lostAt, _ := f.st.lost(); lostAt != nil || len(f.rr.replaced) != 0 || f.lr.proxyStopCount() != stops {
			t.Errorf("the refusal changed something: lost %v, %d replaces, %d stops", lostAt, len(f.rr.replaced), f.lr.proxyStopCount()-stops)
		}
	}
	withRunnerVersion := func(t *testing.T, version string) *readbackFixture {
		f := newEgressEditFixture(t)
		rid := uuid.New()
		f.st.run.RunnerID = &rid
		f.srv.cfg.Store = runnerVersionEditStore{readbackStore: f.rb, version: version}
		return f
	}
	for _, v := range []string{"0.10.0-rc.18", "0.10.0-rc.18+abc123"} {
		t.Run("runner "+v, func(t *testing.T) { refuse(t, withRunnerVersion(t, v)) })
	}
	t.Run("proxy image rc.18", func(t *testing.T) {
		f := newEgressEditFixture(t)
		f.srv.cfg.ProxyImage = "ghcr.io/example/wardyn-proxy:0.10.0-rc.18"
		refuse(t, f)
	})
	for _, v := range []string{"0.10.0-rc.19", "0.10.0-rc.19+abc123", ""} {
		t.Run("runner "+v+" passes the support check", func(t *testing.T) {
			f := withRunnerVersion(t, v)
			run := f.st.run
			if rerr := f.srv.egressEditSupported(context.Background(), run); rerr != nil {
				t.Fatalf("egressEditSupported: %v", rerr)
			}
		})
	}
	t.Run("digest-only proxy image passes the support check", func(t *testing.T) {
		f := newEgressEditFixture(t)
		f.srv.cfg.ProxyImage = "ghcr.io/example/wardyn-proxy@sha256:" + hashHex(nil)
		if rerr := f.srv.egressEditSupported(context.Background(), f.st.run); rerr != nil {
			t.Fatalf("egressEditSupported: %v", rerr)
		}
	})
}
