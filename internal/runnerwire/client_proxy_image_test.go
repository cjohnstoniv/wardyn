// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The proxy image the organisation names in AUTH_OK reaches the runner's hook; an organisation that
// names none leaves it uncalled.
func TestClientTellsTheHookTheOrganisationsProxyImage(t *testing.T) {
	for _, answer := range []string{"org/proxy:2", ""} {
		t.Run("answer="+answer, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			pub, key, _ := ed25519.GenerateKey(rand.Reader)
			const orgURL = "https://org.example"
			row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256(orgURL), State: types.RunnerClaimed}
			org, run := Loopback()
			go func() {
				defer org.Close()
				if _, err := AuthenticateRunner(ctx, org, row, orgURL); err == nil {
					_ = WriteControl(ctx, org, TypeAuthOK, AuthOK{SessionID: uuid.NewString(), PingInterval: PingInterval, ProxyImage: answer})
				}
			}()
			var got []string
			hooks := idleHooks()
			hooks.ProxyImage = func(image string) { got = append(got, image) }
			client, err := NewClient(ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
				Dial: func(context.Context) (Conn, error) { return run, nil }})
			if err != nil {
				t.Fatal(err)
			}
			_ = client.Connect(ctx)
			if want := answer != ""; want != (len(got) == 1 && got[0] == answer) || (!want && len(got) != 0) {
				t.Fatalf("hook saw %q for answer %q", got, answer)
			}
		})
	}
}

// The per-run limits the organisation names in AUTH_OK reach the runner's hook on every connect; an
// organisation that names none tells it 0, 0.
func TestClientTellsTheHookThePerRunLimits(t *testing.T) {
	for _, ok := range []AuthOK{{PerRunCPUMillis: 4000, PerRunMemoryMiB: 8192}, {}} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pub, key, _ := ed25519.GenerateKey(rand.Reader)
		const orgURL = "https://org.example"
		row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256(orgURL), State: types.RunnerClaimed}
		org, run := Loopback()
		go func() {
			defer org.Close()
			if _, err := AuthenticateRunner(ctx, org, row, orgURL); err == nil {
				ok.SessionID, ok.PingInterval = uuid.NewString(), PingInterval
				_ = WriteControl(ctx, org, TypeAuthOK, ok)
			}
		}()
		var got [][2]int64
		hooks := idleHooks()
		hooks.PerRunLimits = func(cpu, mem int64) { got = append(got, [2]int64{cpu, mem}) }
		client, err := NewClient(ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
			Dial: func(context.Context) (Conn, error) { return run, nil }})
		if err != nil {
			t.Fatal(err)
		}
		_ = client.Connect(ctx)
		cancel()
		if want := [][2]int64{{ok.PerRunCPUMillis, ok.PerRunMemoryMiB}}; len(got) != 1 || got[0] != want[0] {
			t.Fatalf("hook saw %v, want %v", got, want)
		}
	}
}
