// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lastGoodControlPlane answers the boot resolve with a credential already
// inside its refresh margin, then answers every re-resolve with next().
func lastGoodControlPlane(t *testing.T, next func() int) (*injector, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
				Host: "api.test", Header: "x-api-key", Value: "sk-last-good-value", JTI: uuid.NewString(),
				ExpiresAt: time.Now().Add(time.Minute).UnixMilli(),
			})
			return
		}
		status := next()
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
				Host: "api.test", Header: "x-api-key", Value: "sk-fresh-value", JTI: uuid.NewString(),
				ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli(),
			})
			return
		}
		http.Error(w, `{"error":"refused"}`, status)
	}))
	t.Cleanup(srv.Close)
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.test"}})
	rules := []InjectionConfig{{InjectionRule: egress.InjectionRule{Host: "api.test", Header: "x-api-key"}, GrantID: uuid.New()}}
	inj, err := buildInjector(context.Background(), srv.URL, newTokenSource("tok"), pol, rules, srv.Client())
	if err != nil {
		t.Fatalf("buildInjector: %v", err)
	}
	return inj, &calls
}

// K8: a store blip (the sink's 503) must not fail a running run's next call.
// The last-good header is served, and the outage is asked again only after
// lastGoodRetry, not on every request.
func TestInjector_TransientFailureServesTheLastGoodHeader(t *testing.T) {
	inj, calls := lastGoodControlPlane(t, func() int { return http.StatusServiceUnavailable })

	for range 3 {
		h, ok, err := inj.resolve("api.test")
		if err != nil || !ok || h.value != "sk-last-good-value" {
			t.Fatalf("resolve during a store outage = %q ok=%v err=%v, want the last-good value", h.value, ok, err)
		}
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("control-plane calls = %d, want 2 (boot + one re-resolve, then paced)", got)
	}
}

// A control plane that does not answer at all is transient too.
func TestInjector_UnreachableControlPlaneServesTheLastGoodHeader(t *testing.T) {
	inj, _ := lastGoodControlPlane(t, func() int { return http.StatusOK })
	inj.base = "http://127.0.0.1:1" // nothing listens
	if h, _, err := inj.resolve("api.test"); err != nil || h.value != "sk-last-good-value" {
		t.Fatalf("resolve with the control plane down = %q err=%v, want the last-good value", h.value, err)
	}
}

// A definitive refusal (the key was removed, the store denied access) stops
// the injection at once, and the dropped header can never come back as
// last-good on a later transient failure.
func TestInjector_DefinitiveRefusalDropsTheHeaderAtOnce(t *testing.T) {
	answers := []int{http.StatusFailedDependency, http.StatusServiceUnavailable}
	inj, _ := lastGoodControlPlane(t, func() int {
		s := answers[0]
		if len(answers) > 1 {
			answers = answers[1:]
		}
		return s
	})
	if h, _, err := inj.resolve("api.test"); err == nil {
		t.Fatalf("resolve after a definitive refusal served %q, want an error", h.value)
	}
	if h, _, err := inj.resolve("api.test"); err == nil {
		t.Fatalf("a transient failure after a refusal served %q as last-good, want an error", h.value)
	}
}

// The grace is bounded: 15 minutes past the entry's expiry, the last-good
// header is no longer served.
func TestInjector_LastGoodEndsWithTheGrace(t *testing.T) {
	inj, _ := lastGoodControlPlane(t, func() int { return http.StatusServiceUnavailable })
	inj.byHost["api.test"].expiresAt = time.Now().Add(-lastGoodGrace - time.Second).UnixMilli()
	if h, _, err := inj.resolve("api.test"); err == nil {
		t.Fatalf("resolve past the grace served %q, want an error", h.value)
	}
}

// Recovery: once the store answers again, the fresh value replaces the
// last-good one at the next attempt.
func TestInjector_RecoversAfterTheOutage(t *testing.T) {
	var up atomic.Bool
	inj, _ := lastGoodControlPlane(t, func() int {
		if up.Load() {
			return http.StatusOK
		}
		return http.StatusServiceUnavailable
	})
	if h, _, _ := inj.resolve("api.test"); h.value != "sk-last-good-value" {
		t.Fatalf("during the outage got %q", h.value)
	}
	up.Store(true)
	inj.byHost["api.test"].retryAt = time.Time{}
	if h, _, err := inj.resolve("api.test"); err != nil || h.value != "sk-fresh-value" {
		t.Fatalf("after the outage = %q err=%v, want the fresh value", h.value, err)
	}
}

// The proxy half of the sink tables (internal/api
// TestProviderKeySink_StoreReadFailures and
// TestProviderSubscriptionSink_StoreReadFailures), keyed by the same case
// names and the status the sinks answer each: only err_unavailable's 503 is
// ridden out, and only until expiry + lastGoodGrace; every other answer drops
// the header on the request that got it. A later 200 installs the new value.
func TestInjector_StoreReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"not_found", http.StatusFailedDependency},
		{"pointer_extant_value_absent", http.StatusFailedDependency},
		{"http_401", http.StatusFailedDependency},
		{"http_403", http.StatusFailedDependency},
		{"disabled_or_retired_key", http.StatusFailedDependency},
		{"binding_mismatch", http.StatusFailedDependency},
		{"err_unavailable", http.StatusServiceUnavailable},
		{"unknown_definitive", http.StatusFailedDependency},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var renewed atomic.Bool
			inj, _ := lastGoodControlPlane(t, func() int {
				if renewed.Load() {
					return http.StatusOK
				}
				return tc.status
			})
			h, _, err := inj.resolve("api.test")
			if tc.status == http.StatusServiceUnavailable {
				if err != nil || h.value != "sk-last-good-value" {
					t.Fatalf("transient failure = %q err=%v, want the last-good value", h.value, err)
				}
				e := inj.byHost["api.test"]
				e.expiresAt, e.retryAt = time.Now().Add(-lastGoodGrace-time.Second).UnixMilli(), time.Time{}
				if h, _, err := inj.resolve("api.test"); err == nil || h.value != "" {
					t.Fatalf("past expiry + grace served %q, want the header gone", h.value)
				}
			} else if err == nil || h.value != "" {
				t.Fatalf("definitive %d served %q, want the header gone on this request", tc.status, h.value)
			}
			renewed.Store(true)
			if h, _, err := inj.resolve("api.test"); err != nil || h.value != "sk-fresh-value" {
				t.Fatalf("renewal = %q err=%v, want the new value", h.value, err)
			}
		})
	}
}

// A control-plane clock more than injectRefreshMargin behind the proxy's makes
// every fresh answer look stale on arrival. It used to be re-resolved on every
// request (a mint and a secret.read row each); it is paced like an outage.
func TestInjector_AnAnswerStaleOnArrivalIsPaced(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Host: "api.test", Header: "x-api-key", Value: "sk-skewed-clock-value", JTI: uuid.NewString(),
			ExpiresAt: time.Now().Add(10*time.Minute - 9*time.Minute).UnixMilli(), // ten-minute TTL, nine minutes of skew
		})
	}))
	t.Cleanup(srv.Close)
	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.test"}})
	rules := []InjectionConfig{{InjectionRule: egress.InjectionRule{Host: "api.test", Header: "x-api-key"}, GrantID: uuid.New()}}
	inj, err := buildInjector(context.Background(), srv.URL, newTokenSource("tok"), pol, rules, srv.Client())
	if err != nil {
		t.Fatalf("buildInjector: %v", err)
	}
	for range 3 {
		if h, _, err := inj.resolve("api.test"); err != nil || h.value != "sk-skewed-clock-value" {
			t.Fatalf("resolve = %q err=%v", h.value, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("control-plane calls = %d, want 2 (boot + one re-resolve, then paced)", got)
	}
}
