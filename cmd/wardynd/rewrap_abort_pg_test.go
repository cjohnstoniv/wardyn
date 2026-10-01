// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
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

func (k *abortingRewrapKEK) LatestVersion(context.Context) (string, error) { return "2", nil }
func (k *abortingRewrapKEK) WrapVersion([]byte) (string, error)            { return "1", nil }

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
	err = rewrapKeys(t.Context(), rec, secretstore.Deps{Pool: pool, AgeIdentity: id, KEK: cred, KEKWrites: true, PlatformKEK: plat, PlatformKEKWrites: true, AdoptBootKeys: true})
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
	d := secretstore.Deps{Pool: pool, AgeIdentity: id, KEK: cred, KEKWrites: true, PlatformKEK: plat, PlatformKEKWrites: true, AdoptBootKeys: true}
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

// Every boot key still under the credential key moves onto the platform key in
// one -rewrap -rewrap-adopt-boot-keys (the first move), a credential stays put,
// and a second run moves nothing. Without the flag the same run refuses, with
// nothing moved.
func TestRewrapKeys_FirstMoveTakesEveryBootKeyUnderTheCredentialKey(t *testing.T) {
	pool := envelopeDB(t)
	cred, plat := newMemKEK(), platformMemKEK{newMemKEK()}
	credStore, err := buildSecretStore(t.Context(), pool, "", nil, "", storeClients{kek: cred, kekWrites: true}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"wardyn-signing-key", "wardyn-session-key", "a-credential"}
	for _, name := range names {
		if err := credStore.Put(t.Context(), name, []byte("v-"+name)); err != nil {
			t.Fatal(err)
		}
		if got := rewrapKEKID(t, pool, name); got != cred.ID() {
			t.Fatalf("%s is sealed under %q, want the credential key", name, got)
		}
	}
	d := secretstore.Deps{Pool: pool, KEK: cred, KEKWrites: true, PlatformKEK: plat, PlatformKEKWrites: true}
	before := rewrapRows(t, pool)
	rec := &capturingRecorder{}
	if err := rewrapKeys(t.Context(), rec, d); !errors.Is(err, secretstorepg.ErrAdoptNotRequested) {
		t.Fatalf("-rewrap without -rewrap-adopt-boot-keys = %v; want it refused", err)
	}
	assertRefusalAudit(t, rec, "adopt_not_requested")
	if rewrapRows(t, pool) != before {
		t.Fatal("a refused -rewrap changed rows")
	}
	d.AdoptBootKeys = true
	if err := rewrapKeys(t.Context(), &capturingRecorder{}, d); err != nil {
		t.Fatalf("first move = %v; want every boot key moved", err)
	}
	for name, want := range map[string]string{"wardyn-signing-key": plat.ID(), "wardyn-session-key": plat.ID(), "a-credential": cred.ID()} {
		if got := rewrapKEKID(t, pool, name); got != want {
			t.Fatalf("%s is sealed under %q after the move, want %q", name, got, want)
		}
	}
	if err := rewrapKeys(t.Context(), &capturingRecorder{}, d); err != nil {
		t.Fatalf("second run = %v; want nothing to move and no refusal", err)
	}
}

// assertRefusalAudit checks the one secret.rewrap row a refused run wrote:
// failure, reason refused, the refusal, and no row name.
func assertRefusalAudit(t *testing.T, rec *capturingRecorder, refusal string) {
	t.Helper()
	if len(rec.got) != 1 || rec.got[0].Outcome != "failure" {
		t.Fatalf("audit events = %+v; want one failure", rec.got)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.got[0].Data, &data); err != nil || data["reason"] != "refused" || data["refusal"] != refusal || data["secrets"] != float64(0) {
		t.Fatalf("audit fields = %s (%v); want reason refused, refusal %q, nothing moved", rec.got[0].Data, err, refusal)
	}
	if strings.Contains(string(rec.got[0].Data), "wardyn-") {
		t.Fatalf("audit fields %s name a row", rec.got[0].Data)
	}
}

// plantUnderCredentialKey writes name under the credential key, as someone
// holding that key's token and write access to the table can.
func plantUnderCredentialKey(t *testing.T, pool *pgxpool.Pool, cred *memKEK, name string) {
	t.Helper()
	credStore, err := buildSecretStore(t.Context(), pool, "", nil, "", storeClients{kek: cred, kekWrites: true}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if err := credStore.Put(t.Context(), name, []byte("forged")); err != nil {
		t.Fatal(err)
	}
	if got := rewrapKEKID(t, pool, name); got != cred.ID() {
		t.Fatalf("the planted row is under %q, want the credential key", got)
	}
}

// A boot key planted under the credential key after the first move is refused
// by -rewrap, with and without -rewrap-adopt-boot-keys (the mixed state is
// what it reports first), and no row changes:
// not the planted one, not the boot keys already on the platform key.
func TestRewrapKeys_RefusesABootKeyPlantedUnderTheCredentialKey(t *testing.T) {
	pool := envelopeDB(t)
	cred, plat := newMemKEK(), platformMemKEK{newMemKEK()}
	id := seedPlatformSplit(t, pool, cred, plat)
	plantUnderCredentialKey(t, pool, cred, "wardyn-session-key")
	before := rewrapRows(t, pool)

	for _, adopt := range []bool{false, true} {
		rec := &capturingRecorder{}
		d := secretstore.Deps{Pool: pool, AgeIdentity: id, KEK: cred, KEKWrites: true, PlatformKEK: plat, PlatformKEKWrites: true, AdoptBootKeys: adopt}
		err := rewrapKeys(t.Context(), rec, d)
		if !errors.Is(err, secretstorepg.ErrMixedBootKeys) || !strings.Contains(err.Error(), "wardyn-session-key") || !strings.Contains(err.Error(), "find out who wrote") {
			t.Fatalf("-rewrap (adopt=%v) with a boot key planted under the credential key = %v; want a named refusal that says to investigate", adopt, err)
		}
		assertRefusalAudit(t, rec, "mixed_boot_keys")
		if rewrapRows(t, pool) != before {
			t.Fatalf("a refused -rewrap (adopt=%v) changed rows", adopt)
		}
	}
	if got := rewrapKEKID(t, pool, "wardyn-session-key"); got != cred.ID() {
		t.Fatalf("the planted row was promoted onto %q", got)
	}
}

// The attacker may delete the boot keys under the platform key first, so what
// -rewrap sees looks like the first move. Without -rewrap-adopt-boot-keys it
// still refuses and nothing moves: only the operator, who knows whether the
// keys were ever adopted, can say the move is a first.
func TestRewrapKeys_RefusesDeleteThenPlantWithoutAdoptFlag(t *testing.T) {
	pool := envelopeDB(t)
	cred, plat := newMemKEK(), platformMemKEK{newMemKEK()}
	id := seedPlatformSplit(t, pool, cred, plat)
	if _, err := pool.Exec(t.Context(), `DELETE FROM secrets WHERE owned_by='' AND name='wardyn-signing-key'`); err != nil {
		t.Fatal(err)
	}
	plantUnderCredentialKey(t, pool, cred, "wardyn-signing-key")
	before := rewrapRows(t, pool)

	rec := &capturingRecorder{}
	d := secretstore.Deps{Pool: pool, AgeIdentity: id, KEK: cred, KEKWrites: true, PlatformKEK: plat, PlatformKEKWrites: true}
	err := rewrapKeys(t.Context(), rec, d)
	if !errors.Is(err, secretstorepg.ErrAdoptNotRequested) || !strings.Contains(err.Error(), "wardyn-signing-key") ||
		!strings.Contains(err.Error(), "investigate before moving anything") {
		t.Fatalf("-rewrap after delete+plant = %v; want a named refusal that says to investigate", err)
	}
	assertRefusalAudit(t, rec, "adopt_not_requested")
	if rewrapRows(t, pool) != before {
		t.Fatal("a refused -rewrap changed rows")
	}
}

// -rewrap-retire-platform-key is checked for a mixed state too: no legitimate
// retire starts with a boot key under the credential key beside platform ones.
func TestRewrapKeys_RetireRefusesAMixedState(t *testing.T) {
	pool := envelopeDB(t)
	cred, plat := newMemKEK(), platformMemKEK{newMemKEK()}
	seedPlatformSplit(t, pool, cred, plat)
	plantUnderCredentialKey(t, pool, cred, "wardyn-session-key")
	before := rewrapRows(t, pool)

	rec := &capturingRecorder{}
	err := rewrapKeys(t.Context(), rec, secretstore.Deps{Pool: pool, KEK: cred, KEKWrites: true, PlatformKEK: plat})
	if !errors.Is(err, secretstorepg.ErrMixedBootKeys) {
		t.Fatalf("retire over a mixed state = %v; want it refused", err)
	}
	assertRefusalAudit(t, rec, "mixed_boot_keys")
	if rewrapRows(t, pool) != before {
		t.Fatal("a refused retire changed rows")
	}
}

// The same with the local WARDYN_PLATFORM_KEY_FILE key: a boot key the age
// key derives, planted after the boot keys moved onto the file key, is
// refused by -rewrap with or without the flag; nothing moves.
func TestRewrapKeys_RefusesABootKeyPlantedUnderTheAgeKeyWithAFileKey(t *testing.T) {
	pool := envelopeDB(t)
	id, platform := mustAgeIdentity(t), mustAgeIdentity(t)
	split, err := buildSecretStore(t.Context(), pool, id.String(), platform, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wardyn-signing-key", "wardyn-session-key"} {
		if err := split.Put(t.Context(), name, []byte("v-"+name)); err != nil {
			t.Fatal(err)
		}
	}
	// Someone holding the age key alone overwrites one boot key.
	ageOnly, err := buildSecretStore(t.Context(), pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if err := ageOnly.Put(t.Context(), "wardyn-session-key", []byte("forged")); err != nil {
		t.Fatal(err)
	}
	before := rewrapRows(t, pool)

	for _, adopt := range []bool{false, true} {
		rec := &capturingRecorder{}
		err := rewrapKeys(t.Context(), rec, secretstore.Deps{Pool: pool, AgeIdentity: id, PlatformIdentity: platform, AdoptBootKeys: adopt})
		if !errors.Is(err, secretstorepg.ErrMixedBootKeys) || !strings.Contains(err.Error(), "wardyn-session-key") {
			t.Fatalf("-rewrap (adopt=%v) with a boot key planted under the age key = %v; want a named refusal", adopt, err)
		}
		assertRefusalAudit(t, rec, "mixed_boot_keys")
		if rewrapRows(t, pool) != before {
			t.Fatalf("a refused -rewrap (adopt=%v) changed rows", adopt)
		}
	}
}

// -rewrap-adopt-boot-keys is a mode of -rewrap, and not of the retire.
func TestRewrapAdoptBootKeys_Refusals(t *testing.T) {
	f := rekeyFlags("", "", "")
	f.rewrap = new(bool)
	*f.rewrapAdoptBootKeys = true
	if ran, err := maintenanceMode(f); !ran || err == nil || !strings.Contains(err.Error(), "is a mode of -rewrap") {
		t.Fatalf("-rewrap-adopt-boot-keys alone = (%v, %v); want a refusal", ran, err)
	}
	f = rekeyFlags("postgres://nobody@127.0.0.1:1/nope?connect_timeout=1", "", newIdentity(t).String())
	*f.rewrapAdoptBootKeys, *f.rewrapRetirePlatformKey = true, true
	if err := rewrapMode(f); err == nil || !strings.Contains(err.Error(), "run one at a time") {
		t.Fatalf("-rewrap-adopt-boot-keys with -rewrap-retire-platform-key = %v; want a refusal", err)
	}
}

// rotatingKEK is a versioned key service whose key rotates once, right after
// the run reads its latest version: the wraps that follow name version 2 while
// the version read first, the one the run would report, was 1.
type rotatingKEK struct {
	*memKEK
	cur   int
	reads int
}

func (k *rotatingKEK) LatestVersion(context.Context) (string, error) {
	k.reads++
	v := strconv.Itoa(k.cur)
	if k.reads == 1 {
		k.cur++
	}
	return v, nil
}

func (k *rotatingKEK) Wrap(ctx context.Context, dek []byte, bind map[string]string) ([]byte, error) {
	w, err := k.memKEK.Wrap(ctx, dek, bind)
	return append([]byte(strconv.Itoa(k.cur)+":"), w...), err
}

func (k *rotatingKEK) Unwrap(ctx context.Context, wrapped []byte, bind map[string]string) ([]byte, error) {
	_, w, _ := bytes.Cut(wrapped, []byte(":"))
	return k.memKEK.Unwrap(ctx, w, bind)
}

func (k *rotatingKEK) WrapVersion(wrapped []byte) (string, error) {
	v, _, _ := bytes.Cut(wrapped, []byte(":"))
	return string(v), nil
}

// A key rotation that lands between the run reading the latest version and its
// wraps leaves rows under a version the run did not report. The run then
// reports no version to retire (the instruction to disable every other version
// would disable the one just used), says so, and the next run, which reads the
// new latest version, moves nothing and reports it.
func TestRewrapKeys_RotationMidRunRetiresNothing(t *testing.T) {
	pool := envelopeDB(t)
	id := mustAgeIdentity(t)
	local, err := buildSecretStore(t.Context(), pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a-credential", "b-credential"} {
		if err := local.Put(t.Context(), name, []byte("v-"+name)); err != nil {
			t.Fatal(err)
		}
	}
	k := &rotatingKEK{memKEK: newMemKEK(), cur: 1}
	d := secretstore.Deps{Pool: pool, AgeIdentity: id, KEK: k, KEKWrites: true}

	rec := &capturingRecorder{}
	out := captureStdout(t, func() {
		if err := rewrapKeys(t.Context(), rec, d); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(out, "now retires") || !strings.Contains(out, "rotation landed") || !strings.Contains(out, "again") {
		t.Fatalf("a rewrap that saw a rotation printed %q; want no retirement instruction and a request to run again", out)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.got[0].Data, &data); err != nil || data["rotated"] != true || data["key_version"] != nil || data["secrets"] != float64(2) {
		t.Fatalf("audit fields = %s (%v); want rotated true, 2 rows and no key_version", rec.got[0].Data, err)
	}

	rec2 := &capturingRecorder{}
	out = captureStdout(t, func() {
		if err := rewrapKeys(t.Context(), rec2, d); err != nil {
			t.Fatal(err)
		}
	})
	if err := json.Unmarshal(rec2.got[0].Data, &data); err != nil || data["secrets"] != float64(0) || data["rotated"] != nil || data["key_version"] != "2" {
		t.Fatalf("second run audit = %s (%v); want 0 rows, no rotation and key_version 2", rec2.got[0].Data, err)
	}
	if !strings.Contains(out, "version 2") {
		t.Fatalf("second run printed %q; want the retirement step for version 2", out)
	}
}

// captureStdout returns what f wrote to os.Stdout.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	func() {
		defer func() { os.Stdout = old; _ = w.Close() }()
		f()
	}()
	return <-done
}

// Key Vault's retirement instruction names older versions only.
func TestRetireStep_AzureNamesOlderVersionsOnly(t *testing.T) {
	got := retireStep(kek.AzureKeyIDPrefix+"v.vault.azure.net/k/s", "1/2")
	if strings.Contains(got, "every other version") || !strings.Contains(got, "OLDER") {
		t.Fatalf("retireStep = %q; want it to name older versions only", got)
	}
}
