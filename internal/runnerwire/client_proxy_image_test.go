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
