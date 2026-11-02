// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// testGatewayKeyStore is a secretKeyStore that counts its reads and writes.
type testGatewayKeyStore struct {
	row          []byte
	getErr       error
	gets, puts   int
	createLocked int
}

func (s *testGatewayKeyStore) Get(context.Context, string) ([]byte, error) {
	s.gets++
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.row == nil {
		return nil, secretstore.ErrNotFound
	}
	return s.row, nil
}

func (s *testGatewayKeyStore) Put(_ context.Context, _ string, v []byte) error {
	s.puts++
	s.row = v
	return nil
}

func (s *testGatewayKeyStore) boot() bootKeyStore {
	return bootKeyStore{s, func(context.Context) (func(), error) {
		s.createLocked++
		return func() {}, nil
	}}
}

func testGatewayKids(t *testing.T, signer *gatewayidentity.Signer) []string {
	t.Helper()
	body, err := signer.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		t.Fatal(err)
	}
	kids := make([]string, 0, len(set.Keys))
	for _, k := range set.Keys {
		kids = append(kids, k.KeyID)
	}
	return kids
}

// Off: no row exists, no read makes one, and repeated anonymous reads cost one
// query per 30 seconds, not one each.
func TestGatewayIdentityReadNeverMakesAKey(t *testing.T) {
	st := &testGatewayKeyStore{}
	load := st.boot().gatewayIdentity()
	for range 5 {
		signer, err := load(context.Background(), false)
		if signer != nil || err != nil {
			t.Fatalf("no row: %v, %v", signer, err)
		}
	}
	if st.puts != 0 || st.row != nil || st.createLocked != 0 {
		t.Errorf("a read wrote: puts=%d row=%q locks=%d", st.puts, st.row, st.createLocked)
	}
	if st.gets != 1 {
		t.Errorf("gets = %d, want 1: the miss is kept 30 seconds", st.gets)
	}
}

// The first save that turns identity on makes the key, under the create lock;
// the read that follows finds it without waiting out the kept miss, and a
// signer outlives the row's next read.
func TestGatewayIdentityCreateMakesOneKeyThatReadsFind(t *testing.T) {
	st := &testGatewayKeyStore{}
	load := st.boot().gatewayIdentity()
	if signer, _ := load(context.Background(), false); signer != nil {
		t.Fatal("a key before create")
	}
	made, err := load(context.Background(), true)
	if err != nil || made == nil {
		t.Fatalf("create: %v, %v", made, err)
	}
	if st.puts != 1 || st.createLocked != 1 {
		t.Errorf("puts=%d locks=%d, want one of each", st.puts, st.createLocked)
	}
	got, err := load(context.Background(), false)
	if err != nil || got != made {
		t.Errorf("read after create: %v, %v", got, err)
	}
	again, err := st.boot().gatewayIdentity()(context.Background(), true)
	if err != nil || again.KeyID() != made.KeyID() || st.puts != 1 {
		t.Errorf("a second replica's create: kid %v, %v, puts=%d; want the winner's key and no write", again, err, st.puts)
	}
}

// A fault never makes a new key: not a failed decrypt, and not a row that fails
// to parse, an empty one included, even when create asks. loadOrCreateSecret
// writes over a row its valid call refuses, so this is what pins valid. No write,
// and the stored bytes stay as they were.
func TestGatewayIdentityFaultNeverMakesAKey(t *testing.T) {
	corrupt := []byte("not the row")
	for name, st := range map[string]*testGatewayKeyStore{
		"decrypt failure": {row: corrupt, getErr: errors.New("decrypt failed")},
		"corrupt row":     {row: corrupt},
		"empty row":       {row: []byte{}},
	} {
		corrupt := st.row
		for _, create := range []bool{false, true} {
			signer, err := st.boot().gatewayIdentity()(context.Background(), create)
			if err == nil || signer != nil {
				t.Errorf("%s, create=%v: %v, %v; want an error", name, create, signer, err)
			}
		}
		if st.puts != 0 || !bytes.Equal(st.row, corrupt) {
			t.Errorf("%s: puts=%d row=%q; want nothing written", name, st.puts, st.row)
		}
		if err := st.boot().rotateGatewayIdentity(context.Background(), &capturingRecorder{}, false, time.Now()); err == nil || st.puts != 0 {
			t.Errorf("%s: rotate = %v, puts=%d; want a refusal and nothing written", name, err, st.puts)
		}
	}
}

// A caller that went away does not hold a failure for the next one.
func TestGatewayIdentityCancelledReadIsNotKept(t *testing.T) {
	st := &testGatewayKeyStore{getErr: context.Canceled}
	load := st.boot().gatewayIdentity()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := load(ctx, false); err == nil {
		t.Fatal("no error from the failed read")
	}
	st.getErr = nil
	if _, err := load(context.Background(), false); err != nil {
		t.Errorf("the next caller got the cancelled caller's failure: %v", err)
	}
	if st.gets != 2 {
		t.Errorf("gets = %d, want 2", st.gets)
	}
}

func TestRotateGatewayIdentityRefusesWithNoKey(t *testing.T) {
	st := &testGatewayKeyStore{}
	rec := &capturingRecorder{}
	err := st.boot().rotateGatewayIdentity(context.Background(), rec, false, time.Now())
	if err == nil || !strings.Contains(err.Error(), "never turned on") {
		t.Errorf("err = %v", err)
	}
	if st.puts != 0 || len(rec.got) != 0 {
		t.Errorf("puts=%d events=%d; want a refusal that leaves nothing", st.puts, len(rec.got))
	}
}

// A planned rotation serves both keys, signs with the new one and says so in a
// row; -retire-previous serves the new key alone, and its row says so.
func TestRotateGatewayIdentityWritesARowAPersonCanFind(t *testing.T) {
	now := time.Date(2025, 11, 2, 12, 0, 0, 0, time.UTC)
	first, err := gatewayidentity.Rotate(nil, false, now)
	if err != nil {
		t.Fatal(err)
	}
	st := &testGatewayKeyStore{row: first}
	old, err := gatewayidentity.New(first)
	if err != nil {
		t.Fatal(err)
	}
	rec := &capturingRecorder{}

	if err := st.boot().rotateGatewayIdentity(context.Background(), rec, false, now); err != nil {
		t.Fatal(err)
	}
	planned, err := gatewayidentity.New(st.row)
	if err != nil {
		t.Fatal(err)
	}
	if got := testGatewayKids(t, planned); !slices.Equal(got, []string{planned.KeyID(), old.KeyID()}) {
		t.Errorf("after a rotation: kids = %v", got)
	}

	if err := st.boot().rotateGatewayIdentity(context.Background(), rec, true, now); err != nil {
		t.Fatal(err)
	}
	dropped, err := gatewayidentity.New(st.row)
	if err != nil {
		t.Fatal(err)
	}
	if got := testGatewayKids(t, dropped); !slices.Equal(got, []string{dropped.KeyID()}) || dropped.KeyID() == planned.KeyID() {
		t.Errorf("after -retire-previous: kids = %v, want only a new key", got)
	}

	if len(rec.got) != 2 {
		t.Fatalf("audit events = %d, want one per rotation", len(rec.got))
	}
	for i, want := range []struct {
		kid, replaced string
		retired       bool
	}{{planned.KeyID(), old.KeyID(), false}, {dropped.KeyID(), planned.KeyID(), true}} {
		ev := rec.got[i]
		if ev.Action != "secret.rekey" || ev.Target != "wardyn-gateway-identity-key" || ev.Outcome != "success" || ev.Actor != "wardyn/rotate-gateway-identity-key" {
			t.Errorf("event %d = %+v", i, ev)
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["kid"] != want.kid || data["replaced_kid"] != want.replaced || data["retired_previous"] != want.retired {
			t.Errorf("event %d data = %v", i, data)
		}
		if _, hasRetire := data["previous_retire_at"]; hasRetire == want.retired {
			t.Errorf("event %d: previous_retire_at present=%v with dropped=%v", i, hasRetire, want.retired)
		}
		if strings.Contains(string(ev.Data), "PRIVATE") {
			t.Errorf("event %d carries key material: %s", i, ev.Data)
		}
	}
}

func testGatewayFlags(dsn string) *bootFlags {
	f := rekeyFlags(dsn, "", "")
	f.rotateAgeKey = new(string)
	f.rewrap, f.rewrapPrincipalKeys, f.migrateOnly = new(bool), new(bool), new(bool)
	f.migrateSecrets, f.reconcile, f.auditSplitLegacy = new(bool), new(bool), new(bool)
	f.rotateGatewayIdentityKey, f.rotateGatewayIdentityRetirePrevious = new(bool), new(bool)
	*f.rotateGatewayIdentityKey = true
	return f
}

// Each refusal fires before anything opens: no database is named or reachable.
func TestRotateGatewayIdentityKeyModeRefusals(t *testing.T) {
	const unreachable = "postgres://nobody@127.0.0.1:1/nope?connect_timeout=1"
	for name, tt := range map[string]struct {
		set  func(f *bootFlags)
		want string
	}{
		"retire alone":    {func(f *bootFlags) { *f.rotateGatewayIdentityKey, *f.rotateGatewayIdentityRetirePrevious = false, true }, "-retire-previous is a mode of"},
		"beside rotate":   {func(f *bootFlags) { *f.rotateAgeKey = "k" }, "mode of its own"},
		"beside rewrap":   {func(f *bootFlags) { *f.rewrap = true }, "mode of its own"},
		"beside migrate":  {func(f *bootFlags) { *f.migrateOnly = true }, "mode of its own"},
		"beside recovery": {func(f *bootFlags) { *f.recoveryVerify = true }, "mode of its own"},
		"no database":     {func(f *bootFlags) { *f.dsn = "" }, "WARDYN_PG_DSN"},
		"unreachable":     {func(*bootFlags) {}, "connect"},
	} {
		t.Run(name, func(t *testing.T) {
			f := testGatewayFlags(unreachable)
			tt.set(f)
			err := rotateGatewayIdentityKeyMode(f)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to name %q", err, tt.want)
			}
		})
	}
}
