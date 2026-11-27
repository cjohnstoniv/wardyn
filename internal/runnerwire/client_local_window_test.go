// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The local socket opens when AUTH_OK allows local attach or a local window, and stays closed only when
// it allows neither: the runner's one hook is told the two answers or'd together on every connect.
func TestClientLocalHookOrsWindow(t *testing.T) {
	for _, c := range []struct{ attach, window, want bool }{
		{false, false, false}, {true, false, true}, {false, true, true}, {true, true, true},
	} {
		t.Run(fmt.Sprintf("attach=%v,window=%v", c.attach, c.window), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			pub, key, _ := ed25519.GenerateKey(rand.Reader)
			const orgURL = "https://org.example"
			row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256(orgURL), State: types.RunnerClaimed}
			org, run := Loopback()
			go func() {
				defer org.Close()
				if _, err := AuthenticateRunner(ctx, org, row, orgURL); err == nil {
					_ = WriteControl(ctx, org, TypeAuthOK, AuthOK{SessionID: uuid.NewString(), PingInterval: PingInterval,
						LocalAttach: c.attach, LocalWindow: c.window})
				}
			}()
			var got []bool
			hooks := idleHooks()
			hooks.LocalAttach = func(allow bool) { got = append(got, allow) }
			client, err := NewClient(ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
				Dial: func(context.Context) (Conn, error) { return run, nil }})
			if err != nil {
				t.Fatal(err)
			}
			_ = client.Connect(ctx)
			if len(got) != 1 || got[0] != c.want {
				t.Fatalf("hook saw %v, want [%v]", got, c.want)
			}
		})
	}
}

// AUTH_OK's local_window and the caps' local_window are optional both ways: an older peer omits them and
// reads as false, and a false one is not sent.
func TestLocalWindowFieldsAreOptionalBothWays(t *testing.T) {
	for _, v := range []any{AuthOK{SessionID: "s"}, Caps{Version: "0.11"}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil || m["local_window"] != nil {
			t.Fatalf("%T sent %s, want no local_window", v, b)
		}
	}
	var ok AuthOK
	if err := json.Unmarshal([]byte(`{"session_id":"s","local_window":true}`), &ok); err != nil || !ok.LocalWindow {
		t.Fatalf("AuthOK = %+v, %v", ok, err)
	}
	if err := (Open{Kind: KindLocalWindow}).Validate(); err != nil {
		t.Fatalf("a local_window stream is refused: %v", err)
	}
	if !runnerToOrgOnly(KindLocalWindow) || callStreamKinds[KindLocalWindow] != MethodLocalWindow || !streamCalls[MethodLocalWindow] {
		t.Fatal("a local_window stream is not the runner-to-org reply of its call")
	}
	end, _ := json.Marshal(LocalWindowEnd{Reason: LocalWindowEndPerson})
	if string(end) != `{"reason":"person"}` {
		t.Fatalf("end line = %s", end)
	}
}
