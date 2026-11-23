// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantFake keeps sessions and events in memory, on the integration double that answers the availability
// reads. finds records every filtered read, so a test can pin the bounds; personReads counts the tool context's
// sign-in reads, the first read a target check makes.
type assistantFake struct {
	*integStore
	store.TemplateStore
	amu         sync.Mutex
	sessions    map[uuid.UUID]types.AssistantSession
	events      map[uuid.UUID][]types.AssistantEvent
	finds       []store.AssistantEventFilter
	templates   []types.RunTemplate
	changes     map[uuid.UUID]types.GovernanceChange
	personReads int
	runIDsErr   error
	sweptBefore *time.Time
}

func newAssistantFake(site types.SiteConfig) *assistantFake {
	return &assistantFake{integStore: &integStore{govEscapeStore: newGovEscapeStore(&capStore{}), site: site},
		sessions: map[uuid.UUID]types.AssistantSession{}, events: map[uuid.UUID][]types.AssistantEvent{},
		changes: map[uuid.UUID]types.GovernanceChange{}}
}

// assistantSessionServer serves the fake with an SSO authenticator, as GET /me/assistant's tests do.
func assistantSessionServer(t *testing.T, f *assistantFake) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, f)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = secretmask.NewRegistry()
	return New(cfg), h
}

// assistantTwoProviders is a site where local-model providers a and b both serve compose on model m1, so
// either can be ready with no credential, and the binding on a.
func assistantTwoProviders() (types.SiteConfig, types.AssistantBinding) {
	a := assistantTestProvider("a", "uid-a", types.ModelProviderLocalModel, "m1")
	b := assistantTestProvider("b", "uid-b", types.ModelProviderLocalModel, "m1")
	site := assistantSiteConfig(&types.AssistantSettings{Compose: true,
		Providers: []types.AssistantProviderMark{assistantTestMark("a", true, false), assistantTestMark("b", true, false)}}, a, b)
	return site, types.AssistantBinding{ProviderID: "a", ProviderUID: "uid-a", Model: "m1"}
}

// assistantLiveSession is a live compose session of person on a new template draft. With no run it is
// fetching, so ending it stops nothing.
func assistantLiveSession(person string, run *uuid.UUID, b types.AssistantBinding) types.AssistantSession {
	state := types.AssistantSessionReady
	if run == nil {
		state = types.AssistantSessionFetching
	}
	return types.AssistantSession{ID: uuid.New(), RunID: run, Principal: person, Email: person + "@corp.example", Role: oidc.RoleUser,
		Purpose: types.AssistantCompose, TargetKind: types.AssistantTargetTemplate, Binding: b, State: state, CreatedAt: time.Now()}
}

func (f *assistantFake) seed(s types.AssistantSession, evs ...types.AssistantEvent) {
	f.amu.Lock()
	defer f.amu.Unlock()
	f.sessions[s.ID] = s
	for _, e := range evs {
		e.SessionID = s.ID
		f.events[s.ID] = append(f.events[s.ID], e)
	}
}

func (f *assistantFake) session(id uuid.UUID) types.AssistantSession {
	f.amu.Lock()
	defer f.amu.Unlock()
	return f.sessions[id]
}

func (f *assistantFake) eventsOf(id uuid.UUID) []types.AssistantEvent {
	f.amu.Lock()
	defer f.amu.Unlock()
	return slices.Clone(f.events[id])
}

// seedRun stores a running run of person.
func (f *assistantFake) seedRun(t *testing.T, person string) uuid.UUID {
	t.Helper()
	run, err := f.CreateRun(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: person, State: types.RunRunning, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return run.ID
}

func (f *assistantFake) lastSeq(id uuid.UUID) int64 {
	evs := f.events[id]
	if len(evs) == 0 {
		return 0
	}
	return evs[len(evs)-1].Seq
}

func (f *assistantFake) clashes(s types.AssistantSession) bool {
	for _, o := range f.sessions {
		if o.Live() && o.ID != s.ID && ((s.RunID != nil && o.RunID != nil && *o.RunID == *s.RunID) || (o.Principal == s.Principal && o.Purpose == s.Purpose)) {
			return true
		}
	}
	return false
}

func (f *assistantFake) CreateAssistantSession(_ context.Context, s types.AssistantSession) (types.AssistantSession, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	if f.clashes(s) {
		return s, store.ErrAssistantSessionLive
	}
	s.CreatedAt = time.Now()
	f.sessions[s.ID] = s
	return s, nil
}

func (f *assistantFake) ReplaceAssistantSession(_ context.Context, old uuid.UUID, s types.AssistantSession, reset []byte) (types.AssistantSession, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	o, ok := f.sessions[old]
	if !ok || !o.Live() {
		return s, store.ErrNotFound
	}
	o.State, o.EndReason = types.AssistantSessionEnded, types.AssistantEndReplaced
	f.sessions[old] = o
	if f.clashes(s) {
		return s, store.ErrAssistantSessionLive
	}
	s.CreatedAt = time.Now()
	f.sessions[s.ID] = s
	if reset != nil {
		f.events[s.ID] = append(f.events[s.ID], types.AssistantEvent{SessionID: s.ID, Seq: f.lastSeq(old) + 1, Kind: types.AssistantEventState, Payload: reset})
	}
	return s, nil
}

func (f *assistantFake) AttachAssistantRun(_ context.Context, session, run uuid.UUID) error {
	f.amu.Lock()
	defer f.amu.Unlock()
	s, ok := f.sessions[session]
	if !ok || s.State != types.AssistantSessionFetching {
		return store.ErrNotFound
	}
	s.RunID, s.State = &run, types.AssistantSessionStarting
	f.sessions[session] = s
	return nil
}

func (f *assistantFake) GetAssistantSession(_ context.Context, id uuid.UUID, principal string) (types.AssistantSession, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	s, ok := f.sessions[id]
	if principal == "" || !ok || s.Principal != principal {
		return types.AssistantSession{}, store.ErrNotFound
	}
	return s, nil
}

func (f *assistantFake) findSession(match func(types.AssistantSession) bool) (types.AssistantSession, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	var out *types.AssistantSession
	for _, s := range f.sessions {
		if match(s) && (out == nil || s.CreatedAt.After(out.CreatedAt)) {
			out = &s
		}
	}
	if out == nil {
		return types.AssistantSession{}, store.ErrNotFound
	}
	return *out, nil
}

func (f *assistantFake) LiveAssistantSession(_ context.Context, principal string, purpose types.AssistantPurpose) (types.AssistantSession, error) {
	return f.findSession(func(s types.AssistantSession) bool {
		return s.Live() && s.Principal == principal && s.Purpose == purpose
	})
}

func (f *assistantFake) LiveAssistantSessionForRun(_ context.Context, run uuid.UUID) (types.AssistantSession, error) {
	return f.findSession(func(s types.AssistantSession) bool { return s.Live() && s.RunID != nil && *s.RunID == run })
}

func (f *assistantFake) LatestAssistantSessionForRun(_ context.Context, run uuid.UUID) (types.AssistantSession, error) {
	return f.findSession(func(s types.AssistantSession) bool { return s.RunID != nil && *s.RunID == run })
}

func (f *assistantFake) AssistantRunIDs(_ context.Context, runs []uuid.UUID) ([]uuid.UUID, error) {
	if f.runIDsErr != nil {
		return nil, f.runIDsErr
	}
	f.amu.Lock()
	defer f.amu.Unlock()
	out := []uuid.UUID{}
	for _, s := range f.sessions {
		if s.RunID != nil && slices.Contains(runs, *s.RunID) && !slices.Contains(out, *s.RunID) {
			out = append(out, *s.RunID)
		}
	}
	return out, nil
}

func (f *assistantFake) AssistantRunIDsCreated(context.Context, time.Time, time.Time) ([]uuid.UUID, error) {
	return nil, nil
}

func (f *assistantFake) AppendAssistantEvent(_ context.Context, session uuid.UUID, kind string, payload []byte) (types.AssistantEvent, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	s, ok := f.sessions[session]
	if !ok {
		return types.AssistantEvent{}, store.ErrNotFound
	}
	if !s.Live() {
		return types.AssistantEvent{}, store.ErrAssistantSessionEnded
	}
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	e := types.AssistantEvent{SessionID: session, Seq: f.lastSeq(session) + 1, Kind: kind, Payload: payload, CreatedAt: time.Now()}
	f.events[session] = append(f.events[session], e)
	return e, nil
}

func (f *assistantFake) AppendAssistantRunEvent(ctx context.Context, session uuid.UUID, kind string, payload []byte) (types.AssistantEvent, error) {
	f.amu.Lock()
	full := len(f.events[session]) >= store.AssistantSessionEventsMax
	f.amu.Unlock()
	if full {
		return types.AssistantEvent{}, store.ErrAssistantSessionFull
	}
	return f.AppendAssistantEvent(ctx, session, kind, payload)
}

func (f *assistantFake) ListAssistantEvents(ctx context.Context, session uuid.UUID, after int64, limit int) ([]types.AssistantEvent, error) {
	return f.FindAssistantEvents(ctx, session, store.AssistantEventFilter{After: after, Limit: limit})
}

func (f *assistantFake) FindAssistantEvents(_ context.Context, session uuid.UUID, q store.AssistantEventFilter) ([]types.AssistantEvent, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	f.finds = append(f.finds, q)
	var picked []types.AssistantEvent
	for _, e := range f.events[session] {
		if e.Seq > q.After && (q.Before <= 0 || e.Seq < q.Before) && (len(q.Kinds) == 0 || slices.Contains(q.Kinds, e.Kind)) {
			picked = append(picked, e)
		}
	}
	n := store.AssistantEventsLimit(q.Limit)
	if len(picked) <= n {
		return picked, nil
	}
	if q.Newest {
		return slices.Clone(picked[len(picked)-n:]), nil
	}
	return slices.Clone(picked[:n]), nil
}

func assistantIsTurn(e types.AssistantEvent) bool {
	var p struct {
		Turn string `json:"turn"`
	}
	_ = json.Unmarshal(e.Payload, &p)
	return e.Kind == types.AssistantEventUserMessage || (e.Kind == types.AssistantEventState && p.Turn == "reset")
}

func (f *assistantFake) NextAssistantTurn(_ context.Context, session uuid.UUID, after int64) (types.AssistantEvent, bool, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	first := true
	for _, e := range f.events[session] {
		if !assistantIsTurn(e) {
			continue
		}
		if e.Seq > after {
			return e, first, nil
		}
		first = false
	}
	return types.AssistantEvent{}, false, store.ErrNotFound
}

func (f *assistantFake) AssistantTurnCount(_ context.Context, session uuid.UUID) (int, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	n := 0
	for _, e := range f.events[session] {
		if e.Kind == types.AssistantEventTurnEnd {
			n++
		}
	}
	return n, nil
}

func (f *assistantFake) SetAssistantDraft(_ context.Context, session uuid.UUID, draft []byte) error {
	f.amu.Lock()
	defer f.amu.Unlock()
	s, ok := f.sessions[session]
	if !ok || !s.Live() {
		return store.ErrNotFound
	}
	s.Draft = draft
	f.sessions[session] = s
	return nil
}

func (f *assistantFake) TouchAssistantSession(_ context.Context, session uuid.UUID, ready bool) error {
	f.amu.Lock()
	defer f.amu.Unlock()
	if s, ok := f.sessions[session]; ok && s.Live() && ready && s.State == types.AssistantSessionStarting {
		s.State = types.AssistantSessionReady
		f.sessions[session] = s
	}
	return nil
}

func (f *assistantFake) EndAssistantSession(_ context.Context, session uuid.UUID, reason string) (bool, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	s, ok := f.sessions[session]
	if !ok || !s.Live() {
		return false, nil
	}
	s.State, s.EndReason = types.AssistantSessionEnded, reason
	f.sessions[session] = s
	return true, nil
}

func (f *assistantFake) EraseAssistantSessionPersonalFields(_ context.Context, principals []string) ([]store.ErasedAssistantSession, int, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	var ended []store.ErasedAssistantSession
	n := 0
	for id, s := range f.sessions {
		if !slices.Contains(principals, s.Principal) {
			continue
		}
		if s.Live() {
			s.State, s.EndReason = types.AssistantSessionEnded, types.AssistantEndErased
			ended = append(ended, store.ErasedAssistantSession{ID: id, RunID: s.RunID})
		}
		s.Principal, s.Email, s.Draft, s.PersonLetters, s.Groups = types.AssistantErasedPrincipal, "", nil, nil, []string{}
		f.sessions[id] = s
		n++
	}
	return ended, n, nil
}

func (f *assistantFake) SweepEndedAssistantSessions(_ context.Context, before time.Time, _ int) (int, error) {
	f.sweptBefore = &before
	return 0, nil
}

func (f *assistantFake) GetPrincipalGroups(context.Context, string, string) (store.PrincipalGroups, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	f.personReads++
	return store.PrincipalGroups{}, store.ErrNotFound
}

func (f *assistantFake) reads() int {
	f.amu.Lock()
	defer f.amu.Unlock()
	return f.personReads
}

func (f *assistantFake) ListUserTypes(context.Context) ([]types.UserType, error) { return nil, nil }

func (f *assistantFake) ListTemplatesVisible(context.Context, string, []string) ([]types.RunTemplate, error) {
	return slices.Clone(f.templates), nil
}

func (f *assistantFake) GetGovernanceChange(_ context.Context, id uuid.UUID) (types.GovernanceChange, error) {
	ch, ok := f.changes[id]
	if !ok {
		return ch, store.ErrNotFound
	}
	return ch, nil
}

// assistantPath is a session route.
func assistantPath(id uuid.UUID, tail string) string {
	return "/api/v1/assistant/sessions/" + id.String() + tail
}

// A session's own binding holds only while its provider, UID, mark, model, grant and credential all hold, whatever
// another provider answers. Availability stays ready in every case where the binding stopped holding, and never on the
// binding's own provider, UID and model; where only b is left a candidate (only), it is ready on b.
func TestAssistantBindingHolds(t *testing.T) {
	newState := func() (*types.AssistantSettings, []types.ModelProvider) {
		set := &types.AssistantSettings{Compose: true, Providers: []types.AssistantProviderMark{
			{ProviderID: "a", Compose: true, Models: []string{"m1", "m2"}}, {ProviderID: "b", Compose: true, Models: []string{"m1"}}}}
		return set, []types.ModelProvider{
			assistantTestProvider("a", "uid-a", types.ModelProviderAnthropicAPIKey, "m1"),
			assistantTestProvider("b", "uid-b", types.ModelProviderAnthropicAPIKey, "m1")}
	}
	bind := types.AssistantBinding{ProviderID: "a", ProviderUID: "uid-a", Model: "m1"}
	never := func(types.ModelProvider) bool { return false }
	all := func(types.ModelProvider) bool { return true }
	notA := func(p types.ModelProvider) bool { return p.ID != "a" }
	cases := []struct {
		name      string
		change    func(*types.AssistantSettings, []types.ModelProvider)
		deny      []string
		allowed   []string
		connected func(types.ModelProvider) bool
		want      string
		only      string // the one provider left a candidate, "" when the binding's provider may still serve a new session
	}{
		{"healthy", func(*types.AssistantSettings, []types.ModelProvider) {}, nil, nil, all, "", ""},
		{"purpose off", func(s *types.AssistantSettings, _ []types.ModelProvider) { s.Compose = false }, nil, nil, all, reasonAssistantOff, ""},
		{"mark removed", func(s *types.AssistantSettings, _ []types.ModelProvider) { s.Providers = s.Providers[1:] }, nil, nil, all, reasonAssistantSessionProviderChanged, "b"},
		{"mark stops serving the purpose", func(s *types.AssistantSettings, _ []types.ModelProvider) { s.Providers[0].Compose = false }, nil, nil, all, reasonAssistantSessionProviderChanged, "b"},
		{"unit denies the provider", func(*types.AssistantSettings, []types.ModelProvider) {}, []string{"uid-a"}, nil, all, reasonAssistantSessionProviderChanged, "b"},
		{"model leaves the mark", func(s *types.AssistantSettings, _ []types.ModelProvider) { s.Providers[0].Models = []string{"m2"} }, nil, nil, all, reasonAssistantSessionProviderChanged, ""},
		{"model leaves the person's allowed models", func(*types.AssistantSettings, []types.ModelProvider) {}, nil, []string{"m2"}, all, reasonAssistantSessionProviderChanged, ""},
		{"UID rotated", func(_ *types.AssistantSettings, ps []types.ModelProvider) { ps[0].UID = "uid-rotated" }, nil, nil, all, reasonAssistantSessionProviderChanged, ""},
		{"credential removed", func(*types.AssistantSettings, []types.ModelProvider) {}, nil, nil, notA, reasonAssistantSessionProviderChanged, "b"},
		{"provider disabled", func(_ *types.AssistantSettings, ps []types.ModelProvider) { ps[0].Disabled = true }, nil, nil, all, reasonAssistantSessionProviderChanged, "b"},
	}
	for _, c := range cases {
		set, ps := newState()
		c.change(set, ps)
		if got := assistantBindingHolds(set, types.AssistantCompose, bind, ps, never, c.deny, c.allowed, c.connected); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if c.want != reasonAssistantSessionProviderChanged {
			continue
		}
		ans := assistantAvailability(set, types.AssistantCompose, ps, never, c.deny, c.allowed, c.connected)
		uid := ""
		if i := slices.IndexFunc(ps, func(p types.ModelProvider) bool { return p.ID == ans.ProviderID }); i >= 0 {
			uid = ps[i].UID
		}
		switch {
		case ans.State != types.AssistantReady:
			t.Errorf("%s: availability = %+v, want ready: the binding stopped holding, another candidate did not", c.name, ans)
		case ans.ProviderID == bind.ProviderID && uid == bind.ProviderUID && ans.Model == bind.Model:
			t.Errorf("%s: availability = %+v on %s, the binding's own provider, UID and model, which no longer hold", c.name, ans, uid)
		case c.only != "" && ans.ProviderID != c.only:
			t.Errorf("%s: availability = %+v, want ready on %s, the only provider left a candidate", c.name, ans, c.only)
		}
	}
}

// A live session's own binding is re-checked on every message and every edit: each change below answers 409 with
// its reason and ends the session as turned_off, while GET /me/assistant, when only the session's provider stopped
// holding, still answers ready, never on the binding's own provider, UID and model, and on b where only b is left.
func TestAssistantMessageAndEditStillAvailable(t *testing.T) {
	change := uuid.New()
	cases := []struct {
		name   string
		alter  func(*types.SiteConfig)
		review bool
		reason string
		ready  bool   // GET /me/assistant still answers ready, never on the binding
		only   string // the one provider left a candidate, "" when the binding's provider may still serve a new session
	}{
		{"purpose off", func(sc *types.SiteConfig) { sc.Assistant.Compose = false }, false, reasonAssistantOff, false, ""},
		{"mark removed from a", func(sc *types.SiteConfig) { sc.Assistant.Providers = sc.Assistant.Providers[1:] }, false, reasonAssistantSessionProviderChanged, true, "b"},
		{"model leaves a's mark", func(sc *types.SiteConfig) { sc.Assistant.Providers[0].Models = []string{"m2"} }, false, reasonAssistantSessionProviderChanged, true, ""},
		{"a's UID rotated", func(sc *types.SiteConfig) { sc.ModelProviders.Providers[0].UID = "uid-rotated" }, false, reasonAssistantSessionProviderChanged, true, ""},
		{"the change's kind becomes never sent", func(sc *types.SiteConfig) { sc.Assistant.NeverSentKinds = []string{govKindUnit} }, true, reasonAssistantKindNeverSent, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			site, bind := assistantTwoProviders()
			if c.review {
				site.Assistant.Review = true
				site.Assistant.Providers[0].Review, site.Assistant.Providers[1].Review = true, true
			}
			f := newAssistantFake(site)
			f.changes[change] = types.GovernanceChange{ID: change, TargetKind: govKindUnit}
			srv, _ := assistantSessionServer(t, f)
			person := "sub-avail-" + uuid.NewString()
			cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
			mk := func() types.AssistantSession {
				s := assistantLiveSession(person+"-"+uuid.NewString(), nil, bind)
				s.Principal = person
				if c.review {
					s.Purpose, s.TargetKind, s.TargetKey = types.AssistantReview, types.AssistantTargetChange, change.String()
				}
				return s
			}
			healthy := mk()
			f.seed(healthy)
			if w := doSSO(t, srv, http.MethodPost, assistantPath(healthy.ID, "/messages"), cookie, `{"text":"hello"}`); w.Code != http.StatusAccepted {
				t.Fatalf("a message while everything holds = %d %s, want 202", w.Code, w.Body)
			}
			f.EndAssistantSession(context.Background(), healthy.ID, types.AssistantEndPerson)
			c.alter(&f.site)
			for _, route := range []struct{ path, body string }{{"/messages", `{"text":"hello"}`}, {"/edits", `{"draft":{},"changed":[]}`}} {
				s := mk()
				f.seed(s)
				wantReason(t, doSSO(t, srv, http.MethodPost, assistantPath(s.ID, route.path), cookie, route.body), http.StatusConflict, c.reason)
				if got := f.session(s.ID); got.Live() || got.EndReason != types.AssistantEndTurnedOff {
					t.Errorf("%s: session = %s/%s, want ended as turned_off", route.path, got.State, got.EndReason)
				}
			}
			if c.ready {
				w := doSSO(t, srv, http.MethodGet, "/api/v1/me/assistant?purpose=compose", cookie, "")
				var ans availabilityAnswer
				if err := json.Unmarshal(w.Body.Bytes(), &ans); err != nil || ans.State != types.AssistantReady {
					t.Fatalf("GET /me/assistant = %d %s, want ready: the binding stopped holding, another candidate did not", w.Code, w.Body)
				}
				p, _ := modelProviderByID(f.site.ModelProviders, ans.ProviderID)
				if ans.ProviderID == bind.ProviderID && p.UID == bind.ProviderUID && ans.Model == bind.Model {
					t.Errorf("GET /me/assistant = %s on %s, the binding's own provider, UID and model, which no longer hold", w.Body, p.UID)
				}
				if c.only != "" && ans.ProviderID != c.only {
					t.Errorf("GET /me/assistant = %s, want ready on %s, the only provider left a candidate", w.Body, c.only)
				}
			}
		})
	}
}

// An edit is capped in fields, field names, values and draft size, and spends from the bucket messages spend.
func TestAssistantEditsAreCappedAndShareTheMessageBucket(t *testing.T) {
	site, bind := assistantTwoProviders()
	f := newAssistantFake(site)
	srv, _ := assistantSessionServer(t, f)
	person := "sub-edits-" + uuid.NewString()
	cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
	s := assistantLiveSession(person, nil, bind)
	f.seed(s)
	path := assistantPath(s.ID, "/edits")
	edit := func(n, value int, draft string) string {
		changed := make([]assistantEditField, n)
		for i := range changed {
			changed[i] = assistantEditField{Field: fmt.Sprintf("f%d", i), Value: strings.Repeat("v", value)}
		}
		b, _ := json.Marshal(map[string]any{"draft": json.RawMessage(draft), "changed": changed})
		return string(b)
	}
	for name, body := range map[string]string{
		"51 fields":             edit(assistantEditFieldsMax+1, 1, `{}`),
		"a value of 8001":       edit(1, assistantEditValueMax+1, `{}`),
		"a draft over 256 KiB":  edit(1, 1, `{"d":"`+strings.Repeat("x", assistantDraftMax)+`"}`),
		"a field name over 200": `{"draft":{},"changed":[{"field":"` + strings.Repeat("f", assistantEditFieldMax+1) + `","value":"v"}]}`,
	} {
		wantReason(t, doSSO(t, srv, http.MethodPost, path, cookie, body), http.StatusBadRequest, reasonInvalidRequestBody)
		if n := len(f.eventsOf(s.ID)); n != 0 {
			t.Fatalf("%s: %d events stored", name, n)
		}
	}
	accepted := 0
	for range 30 {
		w := doSSO(t, srv, http.MethodPost, path, cookie, edit(assistantEditFieldsMax, 10, `{}`))
		if w.Code == http.StatusTooManyRequests {
			break
		}
		if w.Code != http.StatusAccepted {
			t.Fatalf("edit = %d %s", w.Code, w.Body)
		}
		accepted++
	}
	if accepted == 0 || accepted >= 30 {
		t.Fatalf("accepted %d edits in a row, want the bucket to refuse within its burst", accepted)
	}
	wantReason(t, doSSO(t, srv, http.MethodPost, assistantPath(s.ID, "/messages"), cookie, `{"text":"hi"}`), http.StatusTooManyRequests, reasonAssistantRateLimited)
}

// assistantEditField is one changed field as the edits route takes it.
type assistantEditField struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// A turn reads only the person's edits since their last message, and only the latest window of them, however long
// the transcript: past 5000 events the edits after the last message are read and older ones are not re-sent. No
// read asks for more than the read cap, and none reads the transcript from seq 0 as a whole.
func TestAssistantPendingEditsReadABoundedWindow(t *testing.T) {
	site, bind := assistantTwoProviders()
	f := newAssistantFake(site)
	srv, _ := assistantSessionServer(t, f)
	s := assistantLiveSession("sub-window-"+uuid.NewString(), nil, bind)
	var evs []types.AssistantEvent
	edit := func(field, value string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"changed": []assistantEditField{{field, value}}})
		return b
	}
	for i := 1; i <= 6000; i++ {
		e := types.AssistantEvent{Seq: int64(i), Kind: types.AssistantEventText, Payload: json.RawMessage(`{"text":"x"}`)}
		switch {
		case i == 10 || i == 5990:
			e.Kind, e.Payload = types.AssistantEventUserMessage, json.RawMessage(`{"turn":"t"}`)
		case i == 20:
			e.Kind, e.Payload = types.AssistantEventPersonEdit, edit("old", "stale")
		case i == 5995:
			e.Kind, e.Payload = types.AssistantEventPersonEdit, edit("name", "first")
		case i == 5998:
			e.Kind, e.Payload = types.AssistantEventPersonEdit, edit("name", "second")
		case i == 5999:
			e.Kind, e.Payload = types.AssistantEventPersonEdit, edit("hosts", "a.example")
		}
		evs = append(evs, e)
	}
	f.seed(s, evs...)
	got, err := srv.assistantPendingEdits(context.Background(), f, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []assistantEditField{{"name", "second"}, {"hosts", "a.example"}}; len(got) != 2 || got[0].Field != want[0].Field ||
		got[0].Value != want[0].Value || got[1].Field != want[1].Field || got[1].Value != want[1].Value {
		t.Errorf("pending edits = %+v, want %+v", got, want)
	}
	assistantFindsBounded(t, f)
}

// assistantFindsBounded fails when a read asked for more than the read cap, or read from seq 0 more than the one
// record a first turn needs.
func assistantFindsBounded(t *testing.T, f *assistantFake) {
	t.Helper()
	f.amu.Lock()
	defer f.amu.Unlock()
	if len(f.finds) == 0 {
		t.Fatal("no bounded read was made")
	}
	for _, q := range f.finds {
		if q.Limit > store.AssistantEventsReadMax || q.Limit <= 0 {
			t.Errorf("a read asked for %d events, outside 1..%d", q.Limit, store.AssistantEventsReadMax)
		}
		if q.After == 0 && q.Before == 0 && len(q.Kinds) == 0 {
			t.Errorf("a read took the transcript from seq 0 with no kind or bound: %+v", q)
		}
	}
}

// Warm reuse: a relay poll open on the run when the person starts again on the same binding is answered with the
// new session's reset, never end, so the run carries on; the old session ends as replaced and the run is kept. The
// reset carries the records the preview showed, byte for byte.
func TestAssistantWarmReuseAnswersTheOpenPollWithReset(t *testing.T) {
	site, bind := assistantTwoProviders()
	f := newAssistantFake(site)
	f.templates = []types.RunTemplate{{ID: uuid.New(), Name: "deploy", Description: "ships the app"}}
	srv, _ := assistantSessionServer(t, f)
	person := "sub-reuse-" + uuid.NewString()
	cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
	const body = `{"purpose":"compose","target":{"kind":"template"}}`
	w := doSSO(t, srv, http.MethodPost, "/api/v1/assistant/preview", cookie, body)
	var preview types.AssistantPreview
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &preview) != nil || len(preview.Sections) == 0 {
		t.Fatalf("preview = %d %s", w.Code, w.Body)
	}
	run := f.seedRun(t, person)
	old := assistantLiveSession(person, &run, bind)
	f.seed(old, types.AssistantEvent{Seq: 1, Kind: types.AssistantEventState, Payload: json.RawMessage(`{"state":"records","records":[]}`)},
		types.AssistantEvent{Seq: 2, Kind: types.AssistantEventUserMessage, Payload: json.RawMessage(`{"turn":"hi"}`)})

	type polled struct {
		code int
		turn types.AssistantTurn
	}
	done := make(chan polled, 1)
	go func() {
		ctx := context.WithValue(context.Background(), claimsCtxKey{}, &identity.Claims{RunID: run})
		r := httptest.NewRequest(http.MethodGet, "/api/v1/internal/assistant/turns?after=2", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		srv.handleAssistantTurns(rec, r)
		var p polled
		p.code = rec.Code
		_ = json.Unmarshal(rec.Body.Bytes(), &p.turn)
		done <- p
	}()
	time.Sleep(time.Second) // the poll is open and waiting

	w = doSSO(t, srv, http.MethodPost, "/api/v1/assistant/sessions", cookie,
		`{"purpose":"compose","target":{"kind":"template"},"preview_digest":"`+preview.Digest+`"}`)
	var created assistantSessionView
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if created.RunID == nil || *created.RunID != run || created.ID == old.ID {
		t.Fatalf("create = %+v, want a new session on run %s", created, run)
	}
	if got := f.session(old.ID); got.Live() || got.EndReason != types.AssistantEndReplaced {
		t.Errorf("old session = %s/%s, want ended as replaced", got.State, got.EndReason)
	}
	select {
	case p := <-done:
		if p.code != http.StatusOK || p.turn.Kind != types.AssistantTurnReset || p.turn.SessionID != created.ID {
			t.Fatalf("the open poll answered %d %+v, want a reset for session %s", p.code, p.turn, created.ID)
		}
		got, _ := json.Marshal(p.turn.Records)
		want, _ := json.Marshal(preview.Sections)
		if string(got) != string(want) {
			t.Errorf("the reset's records = %s, want the preview's %s", got, want)
		}
	case <-time.After(assistantLongPoll):
		t.Fatal("the open poll never answered")
	}
}

// A create with the admin token alone is refused before anything is read; a preview made stale by one record byte,
// or by availability choosing another provider, answers 409 assistant_preview_stale, and a new preview names the
// provider now serving.
func TestAssistantCreateRefusals(t *testing.T) {
	site, _ := assistantTwoProviders()
	f := newAssistantFake(site)
	f.templates = []types.RunTemplate{{ID: uuid.New(), Name: "deploy", Description: "ships the app"}}
	srv, _ := assistantSessionServer(t, f)
	const target = `"purpose":"compose","target":{"kind":"template"}`
	wantReason(t, do(t, srv, http.MethodPost, "/api/v1/assistant/sessions", adminToken, `{`+target+`,"preview_digest":"x"}`), http.StatusForbidden, reasonAssistantRoleDenied)
	if f.reads() != 0 || len(f.sessions) != 0 {
		t.Fatalf("the admin token alone read %d sign-in records and made %d sessions", f.reads(), len(f.sessions))
	}

	person := "sub-stale-" + uuid.NewString()
	cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
	previewOf := func() types.AssistantPreview {
		w := doSSO(t, srv, http.MethodPost, "/api/v1/assistant/preview", cookie, `{`+target+`}`)
		var p types.AssistantPreview
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatalf("preview = %d %s", w.Code, w.Body)
		}
		return p
	}
	first := previewOf()
	f.templates[0].Description = "ships the apq"
	wantReason(t, doSSO(t, srv, http.MethodPost, "/api/v1/assistant/sessions", cookie, `{`+target+`,"preview_digest":"`+first.Digest+`"}`),
		http.StatusConflict, reasonAssistantPreviewStale)

	second := previewOf()
	f.site.Assistant.Providers = f.site.Assistant.Providers[1:] // availability now picks b; every record byte is the same
	wantReason(t, doSSO(t, srv, http.MethodPost, "/api/v1/assistant/sessions", cookie, `{`+target+`,"preview_digest":"`+second.Digest+`"}`),
		http.StatusConflict, reasonAssistantPreviewStale)
	third := previewOf()
	if third.Provider != "Provider b" || third.Digest == second.Digest {
		t.Errorf("a new preview = %s %s, want provider b and another digest", third.Provider, third.Digest)
	}
	if len(f.sessions) != 0 {
		t.Errorf("a refused create made %d sessions", len(f.sessions))
	}
}

// The person's run limit is at_run_limit; a placement denial answers as the scan launch answers it, never as a
// run limit.
func TestAssistantLaunchRefusal(t *testing.T) {
	srv := &Server{}
	ref := srv.assistantLaunchRefusal(fmt.Errorf("%w: max_concurrent_runs 2", errRecordCeilingLimit))
	if ref.status != http.StatusConflict || ref.reason != reasonAssistantAtRunLimit || ref.custom != nil {
		t.Errorf("run limit = %+v, want 409 %s", ref, reasonAssistantAtRunLimit)
	}
	other := srv.assistantLaunchRefusal(fmt.Errorf("boom"))
	if other.reason == reasonAssistantAtRunLimit || other.custom == nil {
		t.Errorf("another launch error = %+v, want the scan launch's answer, never at_run_limit", other)
	}
}

// Another person's session answers 404 on every {id} route, exactly as a missing one does, and is left as it was:
// no event, no draft, still live.
func TestAssistantAnotherPersonsSessionIsNotFound(t *testing.T) {
	site, bind := assistantTwoProviders()
	f := newAssistantFake(site)
	srv, _ := assistantSessionServer(t, f)
	owner, other := "sub-owner-"+uuid.NewString(), "sub-other-"+uuid.NewString()
	s := assistantLiveSession(owner, nil, bind)
	f.seed(s)
	cookie := ssoSession(t, other, other+"@corp.example", oidc.RoleUser)
	for _, c := range []struct{ method, tail, body string }{
		{http.MethodPost, "/messages", `{"text":"hello"}`},
		{http.MethodPost, "/edits", `{"draft":{"x":1},"changed":[]}`},
		{http.MethodGet, "/events", ""},
		{http.MethodDelete, "", ""},
	} {
		wantReason(t, doSSO(t, srv, c.method, assistantPath(s.ID, c.tail), cookie, c.body), http.StatusNotFound, reasonAssistantSessionNotFound)
		wantReason(t, doSSO(t, srv, c.method, assistantPath(uuid.New(), c.tail), cookie, c.body), http.StatusNotFound, reasonAssistantSessionNotFound)
	}
	got := f.session(s.ID)
	if !got.Live() || len(got.Draft) != 0 || len(f.eventsOf(s.ID)) != 0 {
		t.Errorf("another person's calls changed the session: %s, draft %q, %d events", got.State, got.Draft, len(f.eventsOf(s.ID)))
	}
}

// A provider that leaves prompt inspection to its gateway never serves a create: with every marked provider
// deferring, the create is refused before any session or run exists.
func TestAssistantCreateRefusesADeferringProvider(t *testing.T) {
	site, _ := assistantTwoProviders()
	for i := range site.ModelProviders.Providers {
		site.ModelProviders.Providers[i].GatewayIdentity = &types.ModelProviderGatewayIdentity{Inspection: types.GatewayInspectionGateway}
	}
	f := newAssistantFake(site)
	f.templates = []types.RunTemplate{{ID: uuid.New(), Name: "deploy"}}
	srv, _ := assistantSessionServer(t, f)
	person := "sub-defer-" + uuid.NewString()
	cookie := ssoSession(t, person, person+"@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/assistant/sessions", cookie, `{"purpose":"compose","target":{"kind":"template"},"preview_digest":"x"}`)
	wantReason(t, w, http.StatusConflict, reasonAssistantNoProvider)
	if len(f.sessions) != 0 {
		t.Errorf("a refused create made %d sessions", len(f.sessions))
	}
	if ref := refuseAssistantDeferral(site.ModelProviders.Providers[0]); ref == nil || ref.reason != reasonAssistantDefersInspection {
		t.Errorf("the create's own deferral check = %+v, want %s", ref, reasonAssistantDefersInspection)
	}
}

// A run that ended early is run_ended however late its session is read; one that lived out its lifetime is
// max_lifetime, and so is a live-looking run read past it.
func TestAssistantRunEndReasonMeasuresTheRunsOwnEnd(t *testing.T) {
	created := time.Now().Add(-6 * time.Hour)
	early, full := created.Add(10*time.Minute), created.Add(assistantRunLifetimeSec*time.Second)
	late := time.Now()
	for _, c := range []struct {
		name  string
		ended *time.Time
		want  string
	}{
		{"ended at minute ten, read hours later", &early, types.AssistantEndRunEnded},
		{"ended at its lifetime", &full, types.AssistantEndMaxLifetime},
		{"no end time, read past its lifetime", nil, types.AssistantEndMaxLifetime},
	} {
		if got := assistantRunEndReason(types.AgentRun{CreatedAt: created, EndedAt: c.ended}, late); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
