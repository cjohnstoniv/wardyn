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
