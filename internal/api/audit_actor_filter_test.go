// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// actorTranslatorStub is an Unsealer that also translates a person to a stored
// subject actor.
type actorTranslatorStub struct {
	subjects map[string]string
	err      error
}

func (actorTranslatorStub) Unseal(_ context.Context, evs []types.AuditEvent) ([]types.AuditEvent, error) {
	return evs, nil
}

func (a actorTranslatorStub) StoredActor(_ context.Context, name string) (string, bool, error) {
	s, ok := a.subjects[name]
	return s, ok, a.err
}

var _ audit.ActorTranslator = actorTranslatorStub{}

// ?actor=<person> also matches the subject the person is stored under; the
// name still matches, for rows written before the seal mode was turned on.
func TestParseAuditFilterForTranslatesAPersonToTheirSubject(t *testing.T) {
	srv := &Server{cfg: Config{AuditUnsealer: actorTranslatorStub{subjects: map[string]string{"alice": "subject:0f0e"}}}}
	parse := func(srv *Server, query string) (storeFilter, int) {
		w := httptest.NewRecorder()
		f, ok := srv.parseAuditFilterFor(w, httptest.NewRequest(http.MethodGet, "/api/v1/audit"+query, nil))
		if !ok {
			return storeFilter{}, w.Code
		}
		return storeFilter{actor: f.Actor, alt: f.ActorAlt}, http.StatusOK
	}
	if got, _ := parse(srv, "?actor=alice"); got != (storeFilter{"alice", "subject:0f0e"}) {
		t.Errorf("filter for a person with a subject = %+v, want the name and the subject", got)
	}
	if got, _ := parse(srv, "?actor=nobody"); got != (storeFilter{"nobody", ""}) {
		t.Errorf("filter for a person with none = %+v, want the name alone", got)
	}
	if got, _ := parse(srv, ""); got != (storeFilter{}) {
		t.Errorf("no actor filter = %+v, want none", got)
	}
	// No translator (sealing not armed, or an unsealer that cannot translate): as before.
	if got, _ := parse(&Server{}, "?actor=alice"); got != (storeFilter{"alice", ""}) {
		t.Errorf("filter with no unsealer = %+v, want the name alone", got)
	}
	failing := &Server{cfg: Config{AuditUnsealer: actorTranslatorStub{err: errors.New("directory down")}}}
	if _, code := parse(failing, "?actor=alice"); code < http.StatusInternalServerError {
		t.Errorf("a directory outage answered %d, want a server error rather than a filter that misses the person's rows", code)
	}
}

type storeFilter struct{ actor, alt string }
