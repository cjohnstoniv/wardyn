// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A VM guest's flow that went around the run's proxy settings reaches a via_org host on the relay lane:
// the runner's proxy marks the forwarded request, so the organisation does not count it as the run working.
func TestLaptopProxy_ViaOrgRelayedFlowCarriesTheMark(t *testing.T) {
	for _, relayed := range []bool{true, false} {
		t.Run(laneName(relayed), func(t *testing.T) {
			f := newLaptopFixture(t)
			f.p.vm = true
			var srv *httptest.Server
			if relayed {
				srv = relayServer(f.p)
			} else {
				srv = httptest.NewServer(f.p)
			}
			defer srv.Close()
			req, _ := http.NewRequest(http.MethodPost, "http://api.corp.example/v1/messages", strings.NewReader(`{"q":1}`))
			if body := readAll(t, proxyDo(t, srv.URL, req)); body != "from-org" {
				t.Fatalf("forward = %q, want the organisation's answer", body)
			}
			want := ""
			if relayed {
				want = "1"
			}
			if got := f.cpReq.Header.Get(ViaOrgRelayedHeader); got != want {
				t.Fatalf("%s on the hop = %q, want %q", ViaOrgRelayedHeader, got, want)
			}
		})
	}
}

// A mark the guest sets itself never rides the hop: the agent listener's request, and a relay-lane request
// on a proxy that is not a VM run's, carry neither header to the organisation.
func TestLaptopProxy_ViaOrgGuestSetMarkIsStripped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		vm    bool
		relay bool
	}{{"agent listener, vm run", true, false}, {"relay listener, not a vm run", false, true}, {"agent listener, not a vm run", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaptopFixture(t)
			f.p.vm = tc.vm
			srv := httptest.NewServer(f.p)
			if tc.relay {
				srv.Close()
				srv = relayServer(f.p)
			}
			defer srv.Close()
			req, _ := http.NewRequest(http.MethodPost, "http://api.corp.example/v1/messages", strings.NewReader(`{"q":1}`))
			req.Header.Set(ViaOrgRelayedHeader, "1")
			req.Header.Set(DecisionRelayedHeader, "1")
			if body := readAll(t, proxyDo(t, srv.URL, req)); body != "from-org" {
				t.Fatalf("forward = %q, want the organisation's answer", body)
			}
			if v := f.cpReq.Header.Values(ViaOrgRelayedHeader); len(v) != 0 {
				t.Fatalf("a guest-set %s reached the organisation: %v", ViaOrgRelayedHeader, v)
			}
			if v := f.cpReq.Header.Values(DecisionRelayedHeader); len(v) != 0 {
				t.Fatalf("a guest-set %s reached the organisation: %v", DecisionRelayedHeader, v)
			}
		})
	}
}

// The engine honours the relay lane wardynd puts its request on (WithRelayLane): every decision row of that
// request is posted with DecisionRelayedHeader, and an ordinary request's rows never are.
func TestViaOrgEngine_RelayLaneMarksItsDecisions(t *testing.T) {
	for _, relayed := range []bool{true, false} {
		t.Run(laneName(relayed), func(t *testing.T) {
			f := newViaOrgFixture(t, nil, nil)
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
			r.Host = viaOrgHost
			if relayed {
				r = r.WithContext(WithRelayLane(r.Context()))
			}
			w := httptest.NewRecorder()
			f.engine.ServeInjected(w, r, viaOrgHost)
			if w.Code != http.StatusOK {
				t.Fatalf("engine = %d %s, want 200", w.Code, w.Body)
			}
			if !f.engine.srv.proxy.relayLane(r.Context()) == relayed {
				t.Fatalf("engine relayLane = %v, want %v", !relayed, relayed)
			}
			_ = f.decisions(t)
			f.org.mu.Lock()
			posts, marked := f.org.posts, f.org.relayedPosts
			f.org.mu.Unlock()
			want := 0
			if relayed {
				want = posts
			}
			if posts == 0 || marked != want {
				t.Fatalf("decision posts %d, marked relayed %d, want %d", posts, marked, want)
			}
		})
	}
}

// The mark is wardynd's to set on an engine's request only: a runner's own proxy never reads it.
func TestWithRelayLane_IgnoredOutsideTheEngine(t *testing.T) {
	f := newLaptopFixture(t)
	f.p.vm = true
	if f.p.relayLane(WithRelayLane(context.Background())) {
		t.Fatal("a runner's proxy honoured WithRelayLane")
	}
}
