// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// testRowsStore is a secretKeyStore holding rows by name, for the two gateway rows side by side.
type testRowsStore struct {
	rows map[string][]byte
	puts []string
}

func (s *testRowsStore) Get(_ context.Context, name string) ([]byte, error) {
	if v, ok := s.rows[name]; ok {
		return v, nil
	}
	return nil, secretstore.ErrNotFound
}

func (s *testRowsStore) Put(_ context.Context, name string, v []byte) error {
	s.rows[name] = v
	s.puts = append(s.puts, name)
	return nil
}

func (s *testRowsStore) boot() bootKeyStore {
	return bootKeyStore{s, func(context.Context) (func(), error) { return func() {}, nil }}
}

// The client CA follows the signing key: boot makes it on an install that has the key and never
// on one that does not, once, and a read never makes it.
func TestGatewayClientCAFollowsTheSigningKey(t *testing.T) {
	st := &testRowsStore{rows: map[string][]byte{}}
	load := st.boot().gatewayClientCA()
	st.boot().ensureGatewayClientCA(context.Background(), load)
	if ca, err := load(context.Background(), false); ca != nil || err != nil || len(st.puts) != 0 {
		t.Fatalf("no signing key: ca %v, err %v, puts %v; want nothing made", ca, err, st.puts)
	}
	key, err := gatewayidentity.Rotate(nil, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	st.rows[secretGatewayIdentityKey] = key
	load = st.boot().gatewayClientCA()
	st.boot().ensureGatewayClientCA(context.Background(), load)
	st.boot().ensureGatewayClientCA(context.Background(), st.boot().gatewayClientCA())
	if len(st.puts) != 1 || st.puts[0] != secretGatewayClientCA {
		t.Fatalf("puts %v, want one write of %s", st.puts, secretGatewayClientCA)
	}
	if ca, err := load(context.Background(), false); ca == nil || err != nil {
		t.Fatalf("after boot: ca %v, err %v", ca, err)
	}
}

// A CA row that fails to parse is never made again, even when create asks.
func TestGatewayClientCAFaultNeverMakesACA(t *testing.T) {
	st := &testRowsStore{rows: map[string][]byte{secretGatewayClientCA: []byte("not the row")}}
	for _, create := range []bool{false, true} {
		if ca, err := st.boot().gatewayClientCA()(context.Background(), create); ca != nil || err == nil {
			t.Errorf("create=%v: %v, %v; want an error", create, ca, err)
		}
	}
	if len(st.puts) != 0 {
		t.Errorf("puts %v, want none", st.puts)
	}
}

// The rotation mode rotates the CA after the signing key and writes a row a person can find; with
// no CA row it rotates nothing more and writes no second row.
func TestRotateGatewayClientCA(t *testing.T) {
	now := time.Now()
	first, err := gatewayidentity.RotateAuthority(nil, false, now)
	if err != nil {
		t.Fatal(err)
	}
	old, err := gatewayidentity.NewAuthority(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, retire := range []bool{false, true} {
		st := &testRowsStore{rows: map[string][]byte{secretGatewayClientCA: first}}
		rec := &capturingRecorder{}
		if err := st.boot().rotateGatewayClientCA(context.Background(), rec, retire, now); err != nil {
			t.Fatal(err)
		}
		rotated, err := gatewayidentity.NewAuthority(st.rows[secretGatewayClientCA])
		if err != nil || rotated.Serial() == old.Serial() {
			t.Fatalf("retire=%v: rotated %v, err %v", retire, rotated, err)
		}
		if len(rec.got) != 1 {
			t.Fatalf("retire=%v: %d audit rows, want 1", retire, len(rec.got))
		}
		ev := rec.got[0]
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if ev.Action != "secret.rekey" || ev.Target != "wardyn-gateway-client-ca" || ev.Actor != "wardyn/rotate-gateway-identity-key" ||
			data["serial"] != rotated.Serial() || data["replaced_serial"] != old.Serial() || data["retired_previous"] != retire {
			t.Errorf("retire=%v: event %+v data %v", retire, ev, data)
		}
		if _, has := data["previous_retire_at"]; has == retire {
			t.Errorf("retire=%v: previous_retire_at present=%v", retire, has)
		}
		if strings.Contains(string(ev.Data), "PRIVATE") {
			t.Errorf("the row carries key material: %s", ev.Data)
		}
	}
	st := &testRowsStore{rows: map[string][]byte{}}
	rec := &capturingRecorder{}
	if err := st.boot().rotateGatewayClientCA(context.Background(), rec, false, now); err != nil || len(st.puts) != 0 || len(rec.got) != 0 {
		t.Fatalf("no CA row: err %v, puts %v, rows %d; want nothing", err, st.puts, len(rec.got))
	}
	st = &testRowsStore{rows: map[string][]byte{secretGatewayClientCA: []byte("x")}}
	if err := st.boot().rotateGatewayClientCA(context.Background(), rec, false, now); err == nil || len(st.puts) != 0 {
		t.Fatalf("unreadable CA row: err %v, puts %v; want a refusal that writes nothing", err, st.puts)
	}
}
