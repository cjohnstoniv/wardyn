// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type abortingRewrapKEK struct {
	*memKEK
	calls  int
	cancel context.CancelFunc
}

func (k *abortingRewrapKEK) Wrap(ctx context.Context, dek []byte, bind map[string]string) ([]byte, error) {
	k.calls++
	if k.calls == 2 {
		if k.cancel != nil {
			k.cancel()
		}
		return nil, errors.New("synthetic key-service refusal")
	}
	return k.memKEK.Wrap(ctx, dek, bind)
}

func (k *abortingRewrapKEK) LatestVersion(context.Context) (int, error) { return 2, nil }
func (k *abortingRewrapKEK) WrapVersion([]byte) (int, error)            { return 1, nil }

type rewrapAuditRecorder struct {
	store.Recorder
	ctx context.Context
}

func (r *rewrapAuditRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	r.ctx = ctx
	return r.Recorder.Record(ctx, ev)
}

func TestRewrapKeys_AbortedRunAuditsFailure(t *testing.T) {
	testAbortedRewrap(t, false)
}

func TestRewrapKeys_CanceledContextStillAuditsFailure(t *testing.T) {
	testAbortedRewrap(t, true)
}

func testAbortedRewrap(t *testing.T, cancelDuringWrap bool) {
	t.Helper()
	pool := envelopeDB(t)
	id := mustAgeIdentity(t)
	s, err := buildSecretStore(t.Context(), pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a-private-name", "b-private-name"} {
		if err := s.Put(t.Context(), name, []byte("synthetic-private-value")); err != nil {
			t.Fatal(err)
		}
	}
	before := rewrapRows(t, pool)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	k := &abortingRewrapKEK{memKEK: newMemKEK()}
	if cancelDuringWrap {
		k.cancel = cancel
	}
	rec := &rewrapAuditRecorder{Recorder: store.Recorder{Pool: pool}}
	err = rewrapKeys(ctx, rec, secretstore.Deps{Pool: pool, AgeIdentity: id, KEK: k, KEKWrites: true})
	if err == nil || !strings.Contains(err.Error(), "ABORTED after 1 of 2 rows") || k.calls != 2 {
		t.Fatalf("rewrap = %v after %d wraps; want abort after one updated row", err, k.calls)
	}
	if after := rewrapRows(t, pool); !reflect.DeepEqual(before, after) {
		t.Fatal("aborted rewrap changed committed rows")
	}
	if cancelDuringWrap && !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("rewrap context was not canceled")
	}
	assertRewrapFailureAudit(t, pool)
	if rec.ctx == nil {
		t.Fatal("audit recorder was never called")
	}
	if deadline, ok := rec.ctx.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
		t.Fatalf("audit context has no short deadline: %v, %v", deadline, ok)
	}
}

func rewrapRows(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var rows string
	if err := pool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(s) ORDER BY owned_by, name)::text FROM secrets s`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func assertRewrapFailureAudit(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='secret.rewrap'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("persisted secret.rewrap events = %d; want one", count)
	}
	var actor, actorType, outcome, target string
	var raw []byte
	if err := pool.QueryRow(t.Context(), `SELECT actor, actor_type, outcome, target, data FROM audit_events WHERE action='secret.rewrap'`).Scan(&actor, &actorType, &outcome, &target, &raw); err != nil {
		t.Fatal(err)
	}
	if actor != rewrapActor || actorType != "system" || outcome != "failure" || target != "pg" {
		t.Fatalf("audit identity/outcome = %s %s %s %s", actor, actorType, outcome, target)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"secrets": float64(0), "platform_key_separate": false, "key_service": "transit:transit/test", "reason": "aborted"}
	if !reflect.DeepEqual(data, want) {
		t.Fatalf("audit fields = %s; want committed count zero and no names, values, error text or uncommitted key_version", raw)
	}
}

type platformMemKEK struct{ *memKEK }

func (platformMemKEK) ID() string { return "transit:transit/test-platform" }

// A platform key service counts as a separate platform key: the secret.rewrap
// event says so, and the boot key moves onto it while a credential stays put.
func TestRewrapKeys_PlatformKeyServiceIsSeparate(t *testing.T) {
	pool := envelopeDB(t)
	id := mustAgeIdentity(t)
	s, err := buildSecretStore(t.Context(), pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wardyn-signing-key", "a-credential"} {
		if err := s.Put(t.Context(), name, []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	cred, plat := newMemKEK(), platformMemKEK{newMemKEK()}
	rec := &capturingRecorder{}
	err = rewrapKeys(t.Context(), rec, secretstore.Deps{Pool: pool, AgeIdentity: id, KEK: cred, KEKWrites: true, PlatformKEK: plat, PlatformKEKWrites: true})
	if err != nil || len(rec.got) != 1 {
		t.Fatalf("rewrap = %v with %d audit events", err, len(rec.got))
	}
	var data map[string]any
	if err := json.Unmarshal(rec.got[0].Data, &data); err != nil || data["platform_key_separate"] != true || data["secrets"] != float64(2) {
		t.Fatalf("audit fields = %s (%v); want platform_key_separate true over 2 rows", rec.got[0].Data, err)
	}
	for name, want := range map[string]string{"wardyn-signing-key": plat.ID(), "a-credential": cred.ID()} {
		var got string
		if err := pool.QueryRow(t.Context(), `SELECT kek_id FROM secrets WHERE owned_by='' AND name=$1`, name).Scan(&got); err != nil || got != want {
			t.Fatalf("%s is sealed under %q (%v), want %q", name, got, err, want)
		}
	}
}

// seedPlatformSplit leaves a boot key under plat and a credential under cred,
// as a run of -rewrap with the platform key set does.
func seedPlatformSplit(t *testing.T, pool *pgxpool.Pool, cred *memKEK, plat platformMemKEK) *age.X25519Identity {
	t.Helper()
	id := mustAgeIdentity(t)
	s, err := buildSecretStore(t.Context(), pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wardyn-signing-key", "a-credential"} {
		if err := s.Put(t.Context(), name, []byte("v-"+name)); err != nil {
			t.Fatal(err)
		}
	}
	d := secretstore.Deps{Pool: pool, AgeIdentity: id, KEK: cred, KEKWrites: true, PlatformKEK: plat, PlatformKEKWrites: true}
	if err := rewrapKeys(t.Context(), &capturingRecorder{}, d); err != nil {
		t.Fatal(err)
	}
	return id
}

func rewrapKEKID(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(t.Context(), `SELECT kek_id FROM secrets WHERE owned_by='' AND name=$1`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// -rewrap-retire-platform-key reads the boot keys under the platform key
// (read-only) and writes them under the credential key; a credential row is
// untouched, a second run moves nothing, and a start that still names the
// platform key refuses the boot key it no longer finds there.
func TestRewrapKeys_RetirePlatformKeyToCredentialKey(t *testing.T) {
	pool := envelopeDB(t)
	cred, plat := newMemKEK(), platformMemKEK{newMemKEK()}
	seedPlatformSplit(t, pool, cred, plat)
	var credBefore []byte
	if err := pool.QueryRow(t.Context(), `SELECT wrapped_dek FROM secrets WHERE owned_by='' AND name='a-credential'`).Scan(&credBefore); err != nil {
		t.Fatal(err)
	}

	retire := secretstore.Deps{Pool: pool, KEK: cred, KEKWrites: true, PlatformKEK: plat}
	rec := &capturingRecorder{}
	if err := rewrapKeys(t.Context(), rec, retire); err != nil {
		t.Fatal(err)
	}
	if got := rewrapKEKID(t, pool, "wardyn-signing-key"); got != cred.ID() {
		t.Fatalf("boot key is sealed under %q, want the credential key %q", got, cred.ID())
	}
	var credAfter []byte
	if err := pool.QueryRow(t.Context(), `SELECT wrapped_dek FROM secrets WHERE owned_by='' AND name='a-credential'`).Scan(&credAfter); err != nil || string(credAfter) != string(credBefore) {
		t.Fatalf("the credential row was rewrapped (%v)", err)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.got[0].Data, &data); err != nil || data["secrets"] != float64(1) || data["platform_key_separate"] != false {
		t.Fatalf("audit fields = %s (%v); want 1 row and no separate platform key", rec.got[0].Data, err)
	}

	rec2 := &capturingRecorder{}
	if err := rewrapKeys(t.Context(), rec2, retire); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rec2.got[0].Data, &data); err != nil || data["secrets"] != float64(0) {
		t.Fatalf("second run audit = %s (%v); want nothing moved", rec2.got[0].Data, err)
	}

	// The old env, at a normal start, is still refused.
	s, err := buildSecretStore(t.Context(), pool, "", nil, "", storeClients{kek: cred, kekWrites: true, platformKEK: plat}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(secretstore.WithPurpose(t.Context(), secretstore.PurposeBoot), "wardyn-signing-key"); err == nil || !strings.Contains(err.Error(), "opens boot keys only under") {
		t.Fatalf("Get of a boot key with the platform key still named = %v; want a refusal", err)
	}
	// Without it, the boot key reads under the credential key.
	s, err = buildSecretStore(t.Context(), pool, "", nil, "", storeClients{kek: cred, kekWrites: true}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(secretstore.WithPurpose(t.Context(), secretstore.PurposeBoot), "wardyn-signing-key"); err != nil || string(v) != "v-wardyn-signing-key" {
		t.Fatalf("Get of the boot key after the retire = (%q, %v)", v, err)
	}
}

// With WARDYN_KEK=local the boot keys retire onto the local key (every row
// moves there, as a plain -rewrap back to local does).
func TestRewrapKeys_RetirePlatformKeyToLocalKey(t *testing.T) {
	pool := envelopeDB(t)
	cred, plat := newMemKEK(), platformMemKEK{newMemKEK()}
	id := seedPlatformSplit(t, pool, cred, plat)

	retire := secretstore.Deps{Pool: pool, AgeIdentity: id, KEK: cred, PlatformKEK: plat}
	rec := &capturingRecorder{}
	if err := rewrapKeys(t.Context(), rec, retire); err != nil {
		t.Fatal(err)
	}
	for name, prefix := range map[string]string{"wardyn-signing-key": "local/platform:", "a-credential": "local/cred:"} {
		if got := rewrapKEKID(t, pool, name); !strings.HasPrefix(got, prefix) {
			t.Fatalf("%s is sealed under %q, want %q…", name, got, prefix)
		}
	}
	var data map[string]any
	if err := json.Unmarshal(rec.got[0].Data, &data); err != nil || data["secrets"] != float64(2) || data["platform_key_separate"] != false {
		t.Fatalf("audit fields = %s (%v); want 2 rows and no separate platform key", rec.got[0].Data, err)
	}
	rec2 := &capturingRecorder{}
	if err := rewrapKeys(t.Context(), rec2, retire); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rec2.got[0].Data, &data); err != nil || data["secrets"] != float64(0) {
		t.Fatalf("second run audit = %s (%v); want nothing moved", rec2.got[0].Data, err)
	}
	s, err := buildSecretStore(t.Context(), pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wardyn-signing-key", "a-credential"} {
		if v, err := s.Get(secretstore.WithPurpose(t.Context(), secretstore.PurposeBoot), name); err != nil || string(v) != "v-"+name {
			t.Fatalf("local Get(%s) after the retire = (%q, %v)", name, v, err)
		}
	}
}

// -rewrap-retire-platform-key needs -rewrap and the key to retire.
func TestRewrapRetirePlatformKey_Refusals(t *testing.T) {
	f := rekeyFlags("", "", "")
	f.rewrap = new(bool)
	*f.rewrapRetirePlatformKey = true
	if ran, err := maintenanceMode(f); !ran || err == nil || !strings.Contains(err.Error(), "is a mode of -rewrap") {
		t.Fatalf("-rewrap-retire-platform-key alone = (%v, %v); want a refusal", ran, err)
	}
	ageKey := newIdentity(t).String()
	f = rekeyFlags("postgres://nobody@127.0.0.1:1/nope?connect_timeout=1", "", ageKey)
	*f.rewrapRetirePlatformKey = true
	if err := rewrapMode(f); err == nil || !strings.Contains(err.Error(), "needs WARDYN_VAULT_TRANSIT_KEY_PLATFORM") {
		t.Fatalf("-rewrap-retire-platform-key with no key named = %v; want a refusal", err)
	}
}

// Retiring builds the platform key read-only; a plain -rewrap builds it writing.
func TestWithPlatformKEK(t *testing.T) {
	plat := platformMemKEK{newMemKEK()}
	if d := withPlatformKEK(secretstore.Deps{}, plat, false); d.PlatformKEK == nil || !d.PlatformKEKWrites {
		t.Fatalf("-rewrap: %+v; want the platform key writing", d)
	}
	if d := withPlatformKEK(secretstore.Deps{}, plat, true); d.PlatformKEK == nil || d.PlatformKEKWrites {
		t.Fatalf("-rewrap-retire-platform-key: %+v; want the platform key read-only", d)
	}
	if d := withPlatformKEK(secretstore.Deps{}, nil, true); d.PlatformKEK != nil || d.PlatformKEKWrites {
		t.Fatalf("no platform key: %+v; want none", d)
	}
}
