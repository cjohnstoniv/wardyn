// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"filippo.io/age"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// slowWriterSecrets holds an armed Put until released, after the write has begun and
// before it reaches the store: the slow writer of #1511, whose request is in
// flight while an admin erases the person. It forwards the erase generation, as
// a real store does, which embedding the interface alone would hide.
type slowWriterSecrets struct {
	secretstore.Store
	armed            *atomic.Bool
	reached, release chan struct{}
	once             *sync.Once
}

func (g slowWriterSecrets) For(owner string) secretstore.Store {
	g.Store = g.Store.For(owner)
	return g
}

func (g slowWriterSecrets) Put(ctx context.Context, name string, value []byte) error {
	if g.armed.Load() {
		g.once.Do(func() { close(g.reached) })
		<-g.release
	}
	return g.Store.Put(ctx, name, value)
}

func (g slowWriterSecrets) Generation(ctx context.Context) (int64, error) {
	return g.Store.(secretstore.Generational).Generation(ctx)
}

func (g slowWriterSecrets) AdvanceGeneration(ctx context.Context) (int64, error) {
	return g.Store.(secretstore.Generational).AdvanceGeneration(ctx)
}

// A person's own PUT /secrets/{name} that is in flight while an admin erases
// them never lands: it is answered 409 and the namespace stays empty. A PUT the
// person makes after the erase lands, so reconnecting still works. Real pg
// store; guarded by WARDYN_TEST_PG.
func TestPG_WriteBegunBeforeAnEraseIsRefusedAndAReconnectLands(t *testing.T) {
	pool := throwawayPGPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	inner, err := secretstore.New("pg", secretstore.Deps{Pool: pool, AgeIdentity: id})
	if err != nil {
		t.Fatal(err)
	}
	armed := &atomic.Bool{}
	sec := slowWriterSecrets{Store: secretstore.Audited(inner, &memAudit{}), armed: armed,
		reached: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}
	h, srv := eraseFixture(t, sec)
	bob := ssoSession(t, "bob", "bob@corp.example", oidc.RoleUser)
	admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)

	armed.Store(true)
	type result struct {
		code int
		body string
	}
	inFlight := make(chan result, 1)
	go func() {
		w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/late-key", bob, `{"value":"written-by-the-slow-writer"}`)
		inFlight <- result{w.Code, w.Body.String()}
	}()
	<-sec.reached

	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/people/bob/credentials", admin, ""); w.Code != http.StatusOK {
		t.Fatalf("erase = %d %s", w.Code, w.Body)
	}
	if got := namesOf(t, sec, "bob"); len(got) != 0 {
		t.Fatalf("namespace after the erase = %v", got)
	}
	close(sec.release)
	armed.Store(false)

	got := <-inFlight
	if got.code != http.StatusConflict || !strings.Contains(got.body, reasonCredentialErasedDuringWrite) {
		t.Fatalf("the write that began before the erase = %d %s, want 409 %s", got.code, got.body, reasonCredentialErasedDuringWrite)
	}
	if got := namesOf(t, sec, "bob"); len(got) != 0 {
		t.Fatalf("the refused write left %v behind", got)
	}
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/late-key", bob, `{"value":"reconnected-after-the-erase"}`); w.Code != http.StatusNoContent {
		t.Fatalf("reconnect after the erase = %d %s", w.Code, w.Body)
	}
	if got := namesOf(t, sec, "bob"); !slices.Equal(got, []string{"late-key"}) {
		t.Fatalf("namespace after the reconnect = %v", got)
	}
	ev := lastAuditEvent(t, h.audit.events, "credential.erase")
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil || data["write_fence"] != writeFenceGeneration {
		t.Fatalf("credential.erase data = %s, want write_fence %q", ev.Data, writeFenceGeneration)
	}
}
