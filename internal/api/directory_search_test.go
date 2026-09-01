// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/directory"
)

// fakeDirectory records what the handler asked for and answers with whatever
// the test set. Recording the ARGUMENTS is half the point: several cases below
// assert the connector was never reached at all.
type fakeDirectory struct {
	entries []directory.Entry
	err     error

	calls    int
	lastQ    string
	lastKind directory.Kind
}

func (f *fakeDirectory) Search(_ context.Context, q string, kind directory.Kind) ([]directory.Entry, error) {
	f.calls++
	f.lastQ, f.lastKind = q, kind
	return f.entries, f.err
}

// newDirectorySearchServer builds a server with dir wired (nil is a legitimate
// value — the absent mode) and a FIXED clock, so the rate-limit case below
// controls refill instead of racing the wall clock.
func newDirectorySearchServer(t *testing.T, dir directory.Directory, now func() time.Time) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Directory = dir
	if now != nil {
		cfg.Now = now
	}
	return New(cfg), h
}

func directorySearchPath(q, kind string) string {
	p := "/api/v1/access/directory/search?q=" + q
	if kind != "" {
		p += "&type=" + kind
	}
	return p
}

// TestDirectorySearch is the endpoint's whole contract in one table: the
// absent-mode 503 and its code, the >= MinQueryLen request floor, the kind
// contract, and the "broken" status being a DIFFERENT one from "not
// configured" — which is the distinction the console renders as an error toast
// versus a plain text input, and the one thing a single generic 5xx would
// destroy.
func TestDirectorySearch(t *testing.T) {
	sess := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)

	for _, tc := range []struct {
		name string
		// nil dir = the connector was never wired (WARDYN_DIRECTORY_PROVIDER unset)
		dir      *fakeDirectory
		q, kind  string
		wantCode int
		// wantErrCode is the machine-readable "code" field, "" = must be absent
		wantErrCode string
		// wantCalls is how many times the connector must have been reached
		wantCalls int
		wantKind  directory.Kind
	}{
		{
			name: "no connector configured is a 503 with the absent-mode code",
			// The DEFAULT deployment. Not an error the console shows: it is the
			// signal to degrade the combobox to a free-text input.
			dir: nil, q: "ali",
			wantCode: http.StatusServiceUnavailable, wantErrCode: directoryUnconfiguredCode,
		},
		{
			name: "a connector reporting ErrUnconfigured gets the SAME absent-mode 503",
			// Nothing upstream failed — the feature is off. Unreachable in
			// production (boot refuses a half-configured connector) but the
			// interface permits it, and answering 500 here would be a lie.
			dir: &fakeDirectory{err: directory.ErrUnconfigured}, q: "ali",
			wantCode: http.StatusServiceUnavailable, wantErrCode: directoryUnconfiguredCode,
			wantCalls: 1, wantKind: directory.KindAny,
		},
		{
			name: "an upstream failure is a 500, NOT the absent-mode 503",
			// The distinction this whole test exists for.
			dir: &fakeDirectory{err: &directory.ProviderError{
				Provider: "entra", Op: "users", Status: 403, Err: errors.New("Authorization_RequestDenied"),
			}}, q: "ali",
			wantCode: http.StatusInternalServerError, wantCalls: 1, wantKind: directory.KindAny,
		},
		{
			name: "a plain non-provider error is a 500 too",
			dir:  &fakeDirectory{err: errors.New("boom")}, q: "ali",
			wantCode: http.StatusInternalServerError, wantCalls: 1, wantKind: directory.KindAny,
		},
		{
			name: "a one-character q is refused WITHOUT an upstream call",
			// MinQueryLen is 2: one character matches most of a directory, so the
			// call would spend a Graph round trip to produce noise.
			dir: &fakeDirectory{}, q: "a",
			wantCode: http.StatusBadRequest, wantCalls: 0,
		},
		{
			name: "an empty q is refused",
			dir:  &fakeDirectory{}, q: "",
			wantCode: http.StatusBadRequest, wantCalls: 0,
		},
		{
			name: "a whitespace-padded short q is refused on its TRIMMED length",
			dir:  &fakeDirectory{}, q: "%20a%20",
			wantCode: http.StatusBadRequest, wantCalls: 0,
		},
		{
			name: "a two-RUNE q passes the floor",
			// Length is counted in runes, not bytes: "日本" is 6 bytes and 2
			// characters, and a byte floor would refuse a legitimate query.
			dir: &fakeDirectory{}, q: "%E6%97%A5%E6%9C%AC",
			wantCode: http.StatusOK, wantCalls: 1, wantKind: directory.KindAny,
		},
		{
			name: "an unknown type is refused without an upstream call",
			dir:  &fakeDirectory{}, q: "ali", kind: "manager",
			wantCode: http.StatusBadRequest, wantCalls: 0,
		},
		{
			name: "an explicit kind reaches the connector",
			dir:  &fakeDirectory{}, q: "platform", kind: "group",
			wantCode: http.StatusOK, wantCalls: 1, wantKind: directory.KindGroup,
		},
		{
			name: "an uppercase kind is accepted",
			dir:  &fakeDirectory{}, q: "platform", kind: "GROUP",
			wantCode: http.StatusOK, wantCalls: 1, wantKind: directory.KindGroup,
		},
		{
			name: "an absent type defaults to any — the kind-LESS People-step contract",
			dir:  &fakeDirectory{}, q: "ali",
			wantCode: http.StatusOK, wantCalls: 1, wantKind: directory.KindAny,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A nil *fakeDirectory must reach Config.Directory as a nil
			// INTERFACE, not a typed nil that would pass the != nil check.
			var dir directory.Directory
			if tc.dir != nil {
				dir = tc.dir
			}
			srv, _ := newDirectorySearchServer(t, dir, nil)

			w := doSSO(t, srv, http.MethodGet, directorySearchPath(tc.q, tc.kind), sess, "")
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantCode, w.Body.String())
			}

			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %v (%s)", err, w.Body.String())
			}
			if got, _ := body["code"].(string); got != tc.wantErrCode {
				t.Errorf(`body "code" = %q, want %q`, got, tc.wantErrCode)
			}

			if tc.dir != nil {
				if tc.dir.calls != tc.wantCalls {
					t.Errorf("connector calls = %d, want %d", tc.dir.calls, tc.wantCalls)
				}
				if tc.wantCalls > 0 && tc.dir.lastKind != tc.wantKind {
					t.Errorf("connector kind = %q, want %q", tc.dir.lastKind, tc.wantKind)
				}
			}
		})
	}
}

// TestDirectorySearch_ResultsCarryClaimValue pins the shape the console binds
// to. DisplayName is RENDERED and ClaimValue is STORED, and they are different
// strings for a group on purpose — the GUID-vs-name landmine §I exists to kill —
// so an encoding that dropped or merged either field would silently store the
// wrong subject.
func TestDirectorySearch_ResultsCarryClaimValue(t *testing.T) {
	dir := &fakeDirectory{entries: []directory.Entry{{
		DisplayName: "Platform Engineering",
		ClaimValue:  "8f3c1a2b-0000-4444-9999-abcdefabcdef",
		Kind:        directory.KindGroup,
		Detail:      "group · 8f3c1a2b",
	}}}
	srv, _ := newDirectorySearchServer(t, dir, nil)

	w := doSSO(t, srv, http.MethodGet, directorySearchPath("platform", ""),
		ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got directorySearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(got.Results) != 1 {
		t.Fatalf("results = %d, want 1: %s", len(got.Results), w.Body.String())
	}
	if got.Results[0].ClaimValue != "8f3c1a2b-0000-4444-9999-abcdefabcdef" ||
		got.Results[0].DisplayName != "Platform Engineering" {
		t.Fatalf("entry = %+v, want the GUID stored and the name rendered", got.Results[0])
	}
	// An empty result set must serialize as [] rather than null: a null would
	// make the console distinguish "no matches" from "no results field", which
	// is a difference with no meaning.
	dir.entries = nil
	w = doSSO(t, srv, http.MethodGet, directorySearchPath("zzzz", ""),
		ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin), "")
	if !strings.Contains(w.Body.String(), `"results":[]`) {
		t.Errorf("empty result body = %s, want a []-valued results field", w.Body.String())
	}
}

// TestDirectorySearch_AuditsFailuresNotSearches is §I's audit decision, pinned
// in BOTH directions because only one of them is a security property on its
// own: a connector failure must be visible to the operator who enabled the
// directory read, and a successful search must leave NO row — one audit row per
// keystroke would turn the append-only governance log into a keylogger of who
// an admin looked up.
func TestDirectorySearch_AuditsFailuresNotSearches(t *testing.T) {
	sess := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)

	t.Run("a successful search writes nothing", func(t *testing.T) {
		srv, h := newDirectorySearchServer(t, &fakeDirectory{}, nil)
		before := len(h.audit.events)
		if w := doSSO(t, srv, http.MethodGet, directorySearchPath("alice", ""), sess, ""); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if got := len(h.audit.events) - before; got != 0 {
			t.Fatalf("a successful search wrote %d audit events, want 0", got)
		}
	})

	t.Run("a failure writes one content-free row", func(t *testing.T) {
		srv, h := newDirectorySearchServer(t, &fakeDirectory{err: &directory.ProviderError{
			Provider: "entra", Op: "users", Status: 403, Err: errors.New("Authorization_RequestDenied"),
		}}, nil)
		before := len(h.audit.events)
		// A query that is a person's name, so the assertion below is meaningful.
		if w := doSSO(t, srv, http.MethodGet, directorySearchPath("alice", ""), sess, ""); w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
		}
		events := h.audit.events[before:]
		if len(events) != 1 {
			t.Fatalf("failure wrote %d audit events, want exactly 1", len(events))
		}
		ev := events[0]
		if ev.Action != "directory.search_failed" || ev.Outcome != "failure" {
			t.Errorf("event = {action:%q outcome:%q}, want {directory.search_failed failure}", ev.Action, ev.Outcome)
		}
		if ev.Actor != secAdminSub {
			t.Errorf("actor = %q, want the calling principal %q", ev.Actor, secAdminSub)
		}
		// The typed provider fields, and NOTHING the admin typed.
		data := string(ev.Data)
		for _, want := range []string{`"provider":"entra"`, `"op":"users"`, `"upstream_status":403`} {
			if !strings.Contains(data, want) {
				t.Errorf("audit data %s is missing %s", data, want)
			}
		}
		if strings.Contains(data, "alice") || strings.Contains(ev.Target, "alice") {
			t.Errorf("audit row leaked the searched name: data=%s target=%q", data, ev.Target)
		}
	})
}

// TestDirectorySearch_RateLimitIsPerPrincipal pins the bucket. The endpoint is
// hit once per keystroke and every miss is an upstream Graph call, so it needs
// a limit at all; PER PRINCIPAL is the half that matters, since a global bucket
// would let one admin holding a key down starve every other admin's picker.
func TestDirectorySearch_RateLimitIsPerPrincipal(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	dir := &fakeDirectory{}
	srv, _ := newDirectorySearchServer(t, dir, func() time.Time { return now })

	// Two DIFFERENT security admins. Same tier, same route, separate buckets.
	first := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
	second := ssoSession(t, "sub-sec-two", "sec2@corp.example", oidc.RoleSecurityAdmin)

	// The clock is frozen, so the burst is all anyone gets.
	var allowed int
	for i := 0; i < int(dirBurst)+5; i++ {
		if doSSO(t, srv, http.MethodGet, directorySearchPath("alice", ""), first, "").Code == http.StatusOK {
			allowed++
		}
	}
	if allowed != int(dirBurst) {
		t.Fatalf("first principal got %d requests through, want the burst of %d", allowed, int(dirBurst))
	}
	if w := doSSO(t, srv, http.MethodGet, directorySearchPath("alice", ""), first, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("past the burst: status = %d, want 429; body=%s", w.Code, w.Body.String())
	}

	// The second admin is untouched — the assertion a global bucket fails.
	if w := doSSO(t, srv, http.MethodGet, directorySearchPath("alice", ""), second, ""); w.Code != http.StatusOK {
		t.Fatalf("second principal starved by the first: status = %d; body=%s", w.Code, w.Body.String())
	}

	// And the bucket refills: one second buys dirRatePerSec more.
	now = now.Add(time.Second)
	for i := 0; i < int(dirRatePerSec); i++ {
		if w := doSSO(t, srv, http.MethodGet, directorySearchPath("alice", ""), first, ""); w.Code != http.StatusOK {
			t.Fatalf("refill request %d: status = %d, want 200; body=%s", i, w.Code, w.Body.String())
		}
	}
	if w := doSSO(t, srv, http.MethodGet, directorySearchPath("alice", ""), first, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("past the refill: status = %d, want 429; body=%s", w.Code, w.Body.String())
	}
}
