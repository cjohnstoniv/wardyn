// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// provenanceFake is the session fake with the server's own match: found is the session it names, and asked is
// what the last lookup was given.
type provenanceFake struct {
	*assistantFake
	srv   *Server
	found uuid.UUID
	err   error
	asked struct {
		principal, kind, key string
		endedAfter           time.Time
	}
}

func (f *provenanceFake) DraftingAssistantSession(_ context.Context, principal, kind, key string, endedAfter time.Time) (uuid.UUID, error) {
	f.asked.principal, f.asked.kind, f.asked.key, f.asked.endedAfter = principal, kind, key, endedAfter
	if f.err != nil {
		return uuid.Nil, f.err
	}
	if f.found == uuid.Nil {
		return uuid.Nil, store.ErrNotFound
	}
	return f.found, nil
}

// provenanceCall asks assistantProvenance as person, with header as the session header when it is not empty.
func provenanceCall(t *testing.T, f *provenanceFake, person, header, kind, op, key string) (uuid.UUID, string, bool) {
	t.Helper()
	if f.srv == nil {
		f.srv, _ = assistantSessionServer(t, f.assistantFake)
		f.srv.cfg.Store = f
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r = r.WithContext(withOIDCHuman(r.Context(), person))
	if header != "" {
		r.Header.Set(assistantSessionHeader, header)
	}
	return f.srv.assistantProvenance(r, kind, op, key)
}

// provenanceSeed is a compose session of person on the target, seeded in f.
func provenanceSeed(f *provenanceFake, person string, purpose types.AssistantPurpose, kind, key string) types.AssistantSession {
	_, bind := assistantTwoProviders()
	s := assistantLiveSession(person, nil, bind)
	s.Purpose, s.TargetKind, s.TargetKey = purpose, kind, key
	f.seed(s)
	return s
}

func newProvenanceFake() *provenanceFake {
	site, _ := assistantTwoProviders()
	return &provenanceFake{assistantFake: newAssistantFake(site)}
}

// Only a unit create or update with the session's own unit id is marked among the governance kinds: one row for
// each key of the closed table, so a kind added later with no row here fails.
func TestAssistantProvenanceKinds(t *testing.T) {
	t.Logf("kinds=%d", len(governanceChangeKinds))
	unit := uuid.New().String()
	f := newProvenanceFake()
	for _, kind := range slices.Sorted(maps.Keys(governanceChangeKinds)) {
		t.Run(kind, func(t *testing.T) {
			s := provenanceSeed(f, "sub-own", types.AssistantCompose, types.AssistantTargetUnit, unit)
			id, by, ok := provenanceCall(t, f, "sub-own", s.ID.String(), kind, "update", unit)
			if want := kind == govKindUnit; ok != want {
				t.Fatalf("%s update: stamped = %v, want %v", kind, ok, want)
			}
			if ok && (id != s.ID || by != draftedByHeader) {
				t.Fatalf("stamp = %s %q, want %s %q", id, by, s.ID, draftedByHeader)
			}
		})
	}
	t.Run("unit-delete", func(t *testing.T) {
		s := provenanceSeed(f, "sub-own", types.AssistantCompose, types.AssistantTargetUnit, unit)
		if _, _, ok := provenanceCall(t, f, "sub-own", s.ID.String(), govKindUnit, "delete", unit); ok {
			t.Fatal("a unit delete was stamped")
		}
	})
	t.Run("unit-other-id", func(t *testing.T) {
		s := provenanceSeed(f, "sub-own", types.AssistantCompose, types.AssistantTargetUnit, unit)
		if _, _, ok := provenanceCall(t, f, "sub-own", s.ID.String(), govKindUnit, "update", uuid.NewString()); ok {
			t.Fatal("a unit save of another unit's id was stamped")
		}
	})
	t.Run("run-session-on-unit", func(t *testing.T) {
		s := provenanceSeed(f, "sub-own", types.AssistantCompose, types.AssistantTargetRun, "")
		if _, _, ok := provenanceCall(t, f, "sub-own", s.ID.String(), govKindUnit, "update", unit); ok {
			t.Fatal("a run session stamped a unit proposal")
		}
	})
	t.Run("template-session-on-run", func(t *testing.T) {
		s := provenanceSeed(f, "sub-own", types.AssistantCompose, types.AssistantTargetTemplate, "")
		if _, _, ok := provenanceCall(t, f, "sub-own", s.ID.String(), types.AssistantTargetRun, "create", provenanceNewKey); ok {
			t.Fatal("a template session stamped POST /runs")
		}
	})
}

// The header's shape and the rows of the value table that need no database.
func TestAssistantProvenanceHeader(t *testing.T) {
	tmpl := uuid.New().String()
	cases := []struct {
		name    string
		header  func(s types.AssistantSession) string
		person  string
		purpose types.AssistantPurpose
		kind    string
		key     string
		by      string // "" for not stamped
	}{
		{"own-compose-header", func(s types.AssistantSession) string { return s.ID.String() }, "sub-own", types.AssistantCompose, types.AssistantTargetTemplate, tmpl, draftedByHeader},
		{"header-padded", func(s types.AssistantSession) string { return " " + s.ID.String() + " " }, "sub-own", types.AssistantCompose, types.AssistantTargetTemplate, tmpl, draftedByHeader},
		{"other-person", func(s types.AssistantSession) string { return s.ID.String() }, "sub-other", types.AssistantCompose, types.AssistantTargetTemplate, tmpl, ""},
		{"garbage-id", func(types.AssistantSession) string { return "not-a-uuid" }, "sub-own", types.AssistantCompose, types.AssistantTargetTemplate, tmpl, ""},
		{"unknown-id", func(types.AssistantSession) string { return uuid.NewString() }, "sub-own", types.AssistantCompose, types.AssistantTargetTemplate, tmpl, ""},
		{"own-review", func(s types.AssistantSession) string { return s.ID.String() }, "sub-own", types.AssistantReview, types.AssistantTargetTemplate, tmpl, ""},
		{"own-other-kind", func(s types.AssistantSession) string { return s.ID.String() }, "sub-own", types.AssistantCompose, types.AssistantTargetRun, tmpl, ""},
		{"own-other-template", func(s types.AssistantSession) string { return s.ID.String() }, "sub-own", types.AssistantCompose, types.AssistantTargetTemplate, uuid.NewString(), ""},
		{"no-person", func(s types.AssistantSession) string { return s.ID.String() }, "", types.AssistantCompose, types.AssistantTargetTemplate, tmpl, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newProvenanceFake()
			s := provenanceSeed(f, "sub-own", c.purpose, c.kind, c.key)
			if c.kind == types.AssistantTargetRun {
				s.TargetKey = ""
				f.seed(s)
			}
			id, by, ok := provenanceCall(t, f, c.person, c.header(s), types.AssistantTargetTemplate, "update", tmpl)
			if ok != (c.by != "") || by != c.by {
				t.Fatalf("stamped = %v %q, want %q", ok, by, c.by)
			}
			if ok && id != s.ID {
				t.Fatalf("id = %s, want %s", id, s.ID)
			}
		})
	}
}

// With no usable header the server's own match decides, over the caller's own compose session on the save's
// target, and a header that names a session beats it.
func TestAssistantProvenanceServerMatch(t *testing.T) {
	tmpl := uuid.New().String()
	t.Run("match", func(t *testing.T) {
		f := newProvenanceFake()
		f.found = uuid.New()
		before := time.Now()
		id, by, ok := provenanceCall(t, f, "sub-own", "", types.AssistantTargetTemplate, "update", tmpl)
		if !ok || id != f.found || by != draftedBySession {
			t.Fatalf("stamp = %s %q %v, want %s %q", id, by, ok, f.found, draftedBySession)
		}
		if f.asked.principal != "sub-own" || f.asked.kind != types.AssistantTargetTemplate || f.asked.key != tmpl {
			t.Errorf("asked %+v, want the caller's template session on %s", f.asked, tmpl)
		}
		if got := before.Sub(f.asked.endedAfter); got < assistantDraftWindow-time.Minute || got > assistantDraftWindow+time.Minute {
			t.Errorf("the lookup reaches back %s, want two hours", got)
		}
	})
	t.Run("new-run-asks-for-no-key", func(t *testing.T) {
		f := newProvenanceFake()
		f.found = uuid.New()
		if _, _, ok := provenanceCall(t, f, "sub-own", "", types.AssistantTargetRun, "create", provenanceNewKey); !ok {
			t.Fatal("not stamped")
		}
		if f.asked.kind != types.AssistantTargetRun || f.asked.key != "" {
			t.Errorf("asked %+v, want a run session with no id", f.asked)
		}
	})
	t.Run("no-match", func(t *testing.T) {
		f := newProvenanceFake()
		if _, _, ok := provenanceCall(t, f, "sub-own", "", types.AssistantTargetTemplate, "update", tmpl); ok {
			t.Fatal("stamped with no session")
		}
	})
	t.Run("store-error-is-no-mark", func(t *testing.T) {
		f := newProvenanceFake()
		f.err = errors.New("boom")
		if _, _, ok := provenanceCall(t, f, "sub-own", "", types.AssistantTargetTemplate, "update", tmpl); ok {
			t.Fatal("stamped on a store error")
		}
	})
	t.Run("not-a-draftable-save", func(t *testing.T) {
		f := newProvenanceFake()
		f.found = uuid.New()
		if _, _, ok := provenanceCall(t, f, "sub-own", "", govKindMembership, "update", tmpl); ok {
			t.Fatal("a membership change was stamped")
		}
		if f.asked.kind != "" {
			t.Errorf("the store was asked about a save that can never be drafted: %+v", f.asked)
		}
	})
	t.Run("header-beats-match", func(t *testing.T) {
		f := newProvenanceFake()
		f.found = uuid.New()
		s := provenanceSeed(f, "sub-own", types.AssistantCompose, types.AssistantTargetTemplate, tmpl)
		id, by, ok := provenanceCall(t, f, "sub-own", s.ID.String(), types.AssistantTargetTemplate, "update", tmpl)
		if !ok || id != s.ID || by != draftedByHeader {
			t.Fatalf("stamp = %s %q %v, want the header's %s", id, by, ok, s.ID)
		}
	})
	t.Run("bad-header-falls-to-match", func(t *testing.T) {
		f := newProvenanceFake()
		f.found = uuid.New()
		id, by, ok := provenanceCall(t, f, "sub-own", "not-a-uuid", types.AssistantTargetTemplate, "update", tmpl)
		if !ok || id != f.found || by != draftedBySession {
			t.Fatalf("stamp = %s %q %v, want the server's match", id, by, ok)
		}
	})
}

// provenanceMatches is the closed mapping from a save to the session target it can be drafted from.
func TestProvenanceMatches(t *testing.T) {
	u := uuid.NewString()
	cases := []struct {
		name                   string
		sessionKind, sessionID string
		kind, op, key          string
		want                   bool
	}{
		{"run", "run", "", "run", "create", "new", true},
		{"run-stored-new", "run", "new", "run", "create", "new", true},
		{"run-session-on-a-run", "run", u, "run", "create", "new", false},
		{"run-not-new", "run", "", "run", "create", u, false},
		{"template-create", "template", "", "template", "create", "new", true},
		{"template-update", "template", u, "template", "update", u, true},
		{"template-update-other", "template", uuid.NewString(), "template", "update", u, false},
		{"template-update-new-session", "template", "", "template", "update", u, false},
		{"template-create-from-edit-session", "template", u, "template", "create", "new", false},
		{"unit-update", "unit", u, "governance_unit", "update", u, true},
		{"unit-create", "unit", u, "governance_unit", "create", u, true},
		{"unit-delete", "unit", u, "governance_unit", "delete", u, false},
		{"unit-no-key", "unit", "", "governance_unit", "update", "", false},
		{"change-session", "change", u, "governance_unit", "update", u, false},
		{"other-kind", "unit", u, "governance_membership", "update", u, false},
	}
	for _, c := range cases {
		if got := provenanceMatches(c.sessionKind, c.sessionID, c.kind, c.op, c.key); got != c.want {
			t.Errorf("%s: provenanceMatches = %v, want %v", c.name, got, c.want)
		}
	}
}
