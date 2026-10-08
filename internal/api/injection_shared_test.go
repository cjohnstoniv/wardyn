// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The `shared` grant at the injection sink: the secret an organisation
// provides for its component is read from the operator's namespace and nowhere
// else, and everything after that one read is the tail every stored key takes.

const (
	sharedSecretName = "org-tool-token"
	sharedOperator   = "operator-token-value"
	sharedMember     = "member-token-value"
	sharedHost       = "org-api.example"
	sharedOwner      = "sub-shared-owner"
)

// sinkGrantStore is the two reads the sink makes of the store: the run, for
// the run-token liveness gate, and the run's grants, for the grant's scope.
type sinkGrantStore struct {
	store.Store
	grants    []types.CredentialGrant
	grantsErr error
}

func (s *sinkGrantStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	return types.AgentRun{ID: id, State: types.RunRunning}, nil
}

func (s *sinkGrantStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return s.grants, s.grantsErr
}

// maskPuts is a mask backend that records each committed value and fails the
// commit numbered failAt (0-based; -1 never).
type maskPuts struct {
	*gapCovMaskBackend
	mu     sync.Mutex
	failAt int
	puts   []string
}

func (b *maskPuts) PutRun(_ uuid.UUID, v []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.puts) == b.failAt {
		b.puts = append(b.puts, "")
		return errors.New("mask commit refused")
	}
	b.puts = append(b.puts, string(v))
	return nil
}

type sharedSink struct {
	h     *harness
	st    *sinkGrantStore
	mask  *maskPuts
	reg   *secretmask.Registry
	runID uuid.UUID
	grant uuid.UUID
	token string
}

// newSharedSink is a run of a member who holds a secret under the SAME name
// the operator holds, with one api_key grant whose scope is scope.
func newSharedSink(t *testing.T, scope string, ownerOnly bool) *sharedSink {
	t.Helper()
	h, sec := newSecretsHarness(t)
	if err := sec.Put(t.Context(), sharedSecretName, []byte(sharedOperator)); err != nil {
		t.Fatal(err)
	}
	if err := sec.For(sharedOwner).Put(t.Context(), sharedSecretName, []byte(sharedMember)); err != nil {
		t.Fatal(err)
	}
	s := &sharedSink{h: h, runID: uuid.New(), grant: uuid.New(), mask: &maskPuts{gapCovMaskBackend: &gapCovMaskBackend{}, failAt: -1}}
	s.st = &sinkGrantStore{grants: []types.CredentialGrant{{ID: s.grant, RunID: s.runID,
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(scope), OwnerOnly: ownerOnly}}}}
	s.reg = secretmask.NewRegistry()
	s.reg.SetBackend(s.mask)
	h.srv.cfg.Store, h.srv.cfg.MaskRegistry = s.st, s.reg
	h.srv.router = h.srv.routes()
	s.token = mintRunTokenAs(t, h, s.runID, sharedOwner)
	h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "jti-shared", OwnerOnly: ownerOnly,
		Injection: &egress.InjectionRule{Host: sharedHost, Header: "Authorization", SecretName: sharedSecretName, Format: "Bearer %s"}}
	return s
}

func (s *sharedSink) resolve(t *testing.T) (int, string, injectionResponse) {
	t.Helper()
	rr := do(t, s.h.srv, http.MethodGet, "/api/v1/internal/injection/"+s.grant.String(), s.token, "")
	var resp injectionResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr.Code, rr.Body.String(), resp
}

// reads is the data of every secret.read row for the shared name, by outcome.
func (s *sharedSink) reads(t *testing.T, outcome string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range s.h.audit.snapshot() {
		if ev.Action != "secret.read" || ev.Target != sharedSecretName || ev.Outcome != outcome {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

const sharedScope = `{"host":"org-api.example","require_tls":true,"secret_name":"org-tool-token","shared":true}`

// A shared grant reads the operator's row although the run's owner holds a
// secret of the same name, and the answer is the ordinary stored-key answer:
// formatted, both renderings on the mask record, audited with the operator
// scope, and carrying the stored-key lease the proxy re-resolves on.
func TestInternalInjection_SharedGrantReadsTheOperatorRowThroughTheSameTail(t *testing.T) {
	s := newSharedSink(t, sharedScope, false)
	now := time.Now().UTC().Truncate(time.Second)
	s.h.srv.cfg.Now = func() time.Time { return now }

	code, body, resp := s.resolve(t)
	if code != http.StatusOK {
		t.Fatalf("resolve = %d %s, want 200", code, body)
	}
	if resp.Value != "Bearer "+sharedOperator || resp.Header != "Authorization" || resp.Host != sharedHost || resp.JTI != "jti-shared" {
		t.Errorf("resolved %+v, want the operator's value, formatted", resp)
	}
	if strings.Contains(body, sharedMember) {
		t.Error("the answer carries the member's own secret of the same name")
	}
	if want := now.Add(storedKeyTTL).UnixMilli(); resp.ExpiresAt != want {
		t.Errorf("expires_at = %d, want the stored-key lease %d", resp.ExpiresAt, want)
	}
	// Raw first, then formatted: both committed before the answer was written.
	if !slices.Equal(s.mask.puts, []string{sharedOperator, "Bearer " + sharedOperator}) {
		t.Errorf("mask commits = %q, want the raw value then the formatted one", s.mask.puts)
	}
	leak := []byte("a " + sharedOperator + " b Bearer " + sharedOperator)
	if got := string(s.reg.Masker(s.runID).Mask(leak)); strings.Contains(got, sharedOperator) {
		t.Errorf("the value is not masked for the run: %q", got)
	}
	ok := s.reads(t, "success")
	if len(ok) != 1 || ok[0]["secret_scope"] != "operator" || ok[0]["row_owner"] != "" || ok[0]["jti"] != "jti-shared" ||
		ok[0]["owner"] != sharedOwner || ok[0]["grant_id"] != s.grant.String() || ok[0]["purpose"] != "proxy-injection" {
		t.Errorf("secret.read success rows = %v, want one, stamped with the operator scope and the operator's row", ok)
	}
}

// The tail's own refusals hold for a shared grant: a value that cannot be put
// on the mask record — the raw one, or the formatted one after the raw one
// committed — is not handed out, so both are on record before any answer.
func TestInternalInjection_SharedGrantIsNotAnsweredUntilBothRenderingsAreMasked(t *testing.T) {
	for name, failAt := range map[string]int{"the raw value": 0, "the formatted value": 1} {
		t.Run(name, func(t *testing.T) {
			s := newSharedSink(t, sharedScope, false)
			s.mask.failAt = failAt
			code, body, resp := s.resolve(t)
			if code != authz.EffectUnavailable.Status() || errorReasonOf(body) != string(authz.ReasonMaskStateUnavailable) {
				t.Fatalf("resolve = %d %s, want 503 %s", code, body, authz.ReasonMaskStateUnavailable)
			}
			if resp.Value != "" || strings.Contains(body, sharedOperator) {
				t.Errorf("a value that is not on the mask record was returned: %s", body)
			}
			if got := s.reads(t, "success"); len(got) != 0 {
				t.Errorf("a refused resolve recorded a successful read: %v", got)
			}
		})
	}
}

// No fallback in either direction: a shared grant whose secret the operator
// does not hold is refused even though the run's owner holds one of that name,
// and the failed read is audited with the operator scope.
func TestInternalInjection_SharedGrantNeverFallsBackToTheOwnersRow(t *testing.T) {
	s := newSharedSink(t, sharedScope, false)
	if err := s.h.srv.cfg.Secrets.Delete(t.Context(), sharedSecretName); err != nil {
		t.Fatal(err)
	}
	code, body, resp := s.resolve(t)
	if code != http.StatusFailedDependency || errorReasonOf(body) != reasonSinkSecretNotFound {
		t.Fatalf("resolve = %d %s, want 424 %s", code, body, reasonSinkSecretNotFound)
	}
	if resp.Value != "" || strings.Contains(body, sharedMember) {
		t.Errorf("the member's own secret answered a shared grant: %s", body)
	}
	// The proxy relays this body into the sandbox: it may not say what the
	// organisation's secret is called.
	if strings.Contains(body, sharedSecretName) || !strings.Contains(body, sinkSharedSecretRefused) {
		t.Errorf("the refusal of a shared grant = %s, want the name-free sentence", body)
	}
	failed := s.reads(t, "failure")
	if len(failed) != 1 || failed[0]["secret_scope"] != "operator" || failed[0]["reason"] != reasonSinkSecretNotFound {
		t.Errorf("secret.read failure rows = %v, want one, stamped with the operator scope", failed)
	}
	if len(s.mask.puts) != 0 {
		t.Errorf("a refused read committed %q to the mask record", s.mask.puts)
	}
}

// A grant that does not declare `shared` reads exactly as it did: the owner's
// own row first, and its audit row gains no key.
func TestInternalInjection_AnUnsharedGrantReadsAsBefore(t *testing.T) {
	for name, scope := range map[string]string{
		"no shared key": `{"host":"org-api.example","require_tls":true,"secret_name":"org-tool-token"}`,
		"shared false":  `{"host":"org-api.example","require_tls":true,"secret_name":"org-tool-token","shared":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newSharedSink(t, scope, false)
			code, body, resp := s.resolve(t)
			if code != http.StatusOK || resp.Value != "Bearer "+sharedMember {
				t.Fatalf("resolve = %d %s, want the owner's own row", code, body)
			}
			ok := s.reads(t, "success")
			if len(ok) != 1 || ok[0]["row_owner"] != sharedOwner {
				t.Fatalf("secret.read success rows = %v, want one naming the owner's row", ok)
			}
			if _, stamped := ok[0]["secret_scope"]; stamped {
				t.Errorf("an unshared grant's row carries secret_scope: %v", ok[0])
			}
		})
	}
}

// The scope is read from the run's own grant list. When that list cannot be
// read the sink cannot know whether the grant is shared, and refuses the read
// as it refuses an unreachable store, before any value is read.
func TestInternalInjection_AnUnreadableGrantListRefusesTheRead(t *testing.T) {
	s := newSharedSink(t, sharedScope, false)
	s.st.grantsErr = errors.New("connection refused")
	code, body, resp := s.resolve(t)
	if code != http.StatusServiceUnavailable || errorReasonOf(body) != reasonSinkStoreUnavailable {
		t.Fatalf("resolve = %d %s, want 503 %s", code, body, reasonSinkStoreUnavailable)
	}
	if resp.Value != "" || strings.Contains(body, sharedOperator) || strings.Contains(body, sharedMember) || strings.Contains(body, "connection refused") {
		t.Errorf("the refusal carries a value or the store's own error: %s", body)
	}
	if len(s.mask.puts) != 0 || len(s.reads(t, "success")) != 0 {
		t.Error("a refused resolve read or recorded a value")
	}
}

// A grant the run's list does not hold declares nothing, and reads as every
// grant did before the flag existed.
func TestInternalInjection_AGrantOutsideTheRunsListIsNotShared(t *testing.T) {
	s := newSharedSink(t, sharedScope, false)
	s.st.grants = nil
	code, body, resp := s.resolve(t)
	if code != http.StatusOK || resp.Value != "Bearer "+sharedMember {
		t.Fatalf("resolve = %d %s, want the owner-then-operator read", code, body)
	}
}

func errorReasonOf(body string) string {
	var e struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(body), &e)
	return e.Reason
}

// An unshared grant's refusal is the one it always was.
func TestInternalInjection_AnUnsharedGrantsRefusalIsUnchanged(t *testing.T) {
	s := newSharedSink(t, `{"host":"org-api.example","require_tls":true,"secret_name":"org-tool-token"}`, true)
	// owner_only on a member's run: only their own row, which is removed here.
	if err := s.h.srv.cfg.Secrets.For(sharedOwner).Delete(t.Context(), sharedSecretName); err != nil {
		t.Fatal(err)
	}
	if err := s.h.srv.cfg.Secrets.Delete(t.Context(), sharedSecretName); err != nil {
		t.Fatal(err)
	}
	code, body, _ := s.resolve(t)
	if code != http.StatusFailedDependency || !strings.Contains(body, "secret "+sharedSecretName+" is not in the store") {
		t.Fatalf("resolve = %d %s, want the existing 424 sentence", code, body)
	}
	if failed := s.reads(t, "failure"); len(failed) != 1 || failed[0]["secret_scope"] != nil {
		t.Errorf("secret.read failure rows = %v, want one without secret_scope", failed)
	}
}
