// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestHubRevokedWhileAdmissionWaitsNeverAttaches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"), State: types.RunnerClaimed}
	id := row.ID
	var mu sync.Mutex
	link := &Link{}
	sub := New(id.String(), link, Options{})
	admitting, release := make(chan struct{}), make(chan struct{})
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) {
		mu.Lock()
		defer mu.Unlock()
		return row, nil
	}, Admit: func(context.Context, types.Runner) (Admission, error) {
		close(admitting)
		select {
		case <-release:
		case <-ctx.Done():
			return Admission{}, ctx.Err()
		}
		return Admission{Substrate: sub, Link: link,
			Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
			BeginInventory: func(context.Context, string) error { return nil },
			StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
			Event:          func(context.Context, runnerwire.Event) error { return nil },
		}, nil
	}})
	defer hub.Close()
	org, run := runnerwire.Loopback()
	defer run.Close()
	serverDone, clientDone := make(chan error, 1), make(chan error, 1)
	go func() { serverDone <- hub.Serve(ctx, org, id) }()
	go func() {
		_, err := runnerwire.AuthenticateOrg(ctx, run, id.String(), "https://org.example", key, nil)
		clientDone <- err
	}()
	awaitValue(t, ctx, admitting)
	mu.Lock()
	row.State = types.RunnerRevoked
	mu.Unlock()
	if err := hub.Revoke(ctx, id); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := awaitValue(t, ctx, serverDone); !errors.Is(err, runnerwire.ErrAuth) {
		t.Fatalf("post-revocation admission: %v", err)
	}
	if err := awaitValue(t, ctx, clientDone); err == nil {
		t.Fatal("revoked session received AUTH_OK")
	}
	if link.current() != nil {
		t.Fatal("revoked row attached after slow admission")
	}
}

func TestHubWrongKeyRunnerAndReplayedNonceNeverAttach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"), State: types.RunnerClaimed}
	link := &Link{}
	// Even a valid signature must not attach a different runner's substrate.
	sub := New(uuid.NewString(), link, Options{})
	var admits atomic.Int32
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			admits.Add(1)
			return Admission{Substrate: sub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	})
	defer hub.Close()
	var captured runnerwire.Auth
	for _, mode := range []string{"wrong key", "wrong runner", "wrong substrate", "replayed nonce"} {
		t.Run(mode, func(t *testing.T) {
			org, run := runnerwire.Loopback()
			defer run.Close()
			done := make(chan error, 1)
			go func() { done <- hub.Serve(ctx, org, row.ID) }()
			frame, err := run.ReadFrame(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var hello runnerwire.Hello
			if err := runnerwire.Unmarshal(frame.Payload, &hello); err != nil {
				t.Fatal(err)
			}
			id, signer := row.ID.String(), key
			if mode == "wrong runner" {
				id = uuid.NewString()
			}
			if mode == "wrong key" {
				signer = wrong
			}
			auth := runnerwire.Auth{RunnerID: id, Version: runnerwire.ProtocolVersion, Sig: runnerwire.SignAuth(signer, hello.Nonce, id, hello.OrgURLSHA256)}
			if mode == "wrong substrate" {
				captured = auth
			}
			if mode == "replayed nonce" {
				auth = captured
			}
			if err := runnerwire.WriteControl(ctx, run, runnerwire.TypeAuth, auth); err != nil {
				t.Fatal(err)
			}
			if err := awaitValue(t, ctx, done); err == nil {
				t.Fatal("invalid binding admitted")
			}
			if link.current() != nil {
				t.Fatal("attached before key/runner binding verified")
			}
		})
	}
	if admits.Load() != 1 {
		t.Fatalf("unauthenticated calls reached admission: %d", admits.Load())
	}
}

func TestHubPendingActionsPrecedeStateAndReady(t *testing.T) {
	action := runnerwire.PendingAction{ID: uuid.New()}
	var cfg runnerwire.PeerConfig
	a := Admission{BeginInventory: func(context.Context, string) error { return nil }, StatePage: func(context.Context, string, runnerwire.StatePage) error { return nil }, Event: func(context.Context, runnerwire.Event) error { return nil }}
	configureAdmission(context.Background(), &cfg, false, []runnerwire.PendingAction{action}, a, &hubSession{}, uuid.NewString(), func() *runnerwire.Peer { return nil })
	if err := cfg.OnControlChecked(runnerwire.Frame{Type: runnerwire.TypeReady}); !errors.Is(err, runnerwire.ErrBadFrame) {
		t.Fatalf("READY before pending/state: %v", err)
	}
	state := runnerwire.Frame{Type: runnerwire.TypeState, Payload: emptyStatePage(t)}
	if err := cfg.OnControlChecked(state); !errors.Is(err, runnerwire.ErrBadFrame) {
		t.Fatalf("STATE before pending result: %v", err)
	}
	refused := errors.New("durable event acceptance failed")
	a.Event = func(context.Context, runnerwire.Event) error { return refused }
	configureAdmission(context.Background(), &cfg, false, []runnerwire.PendingAction{action}, a, &hubSession{}, uuid.NewString(), func() *runnerwire.Peer { return nil })
	data, _ := runnerwire.Marshal(runnerwire.ActionResult{ID: action.ID, Outcome: "applied"})
	if err := cfg.OnEventChecked(runnerwire.Event{Kind: runnerwire.EventActionResult, Data: data}); !errors.Is(err, refused) {
		t.Fatal(err)
	}
	if err := cfg.OnControlChecked(state); !errors.Is(err, runnerwire.ErrBadFrame) {
		t.Fatalf("failed durable action unblocked STATE: %v", err)
	}
}

// A refused authentication is ErrAuth and names its cause, so the operator's log line can tell a runner
// that is not registered (or a failing store) from one whose proof did not verify.
func TestHubAuthRefusalNamesItsCause(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"), State: types.RunnerClaimed}
	for name, tc := range map[string]struct {
		lookup func(context.Context, uuid.UUID) (types.Runner, error)
		sign   bool
		want   string
	}{
		"store down": {func(context.Context, uuid.UUID) (types.Runner, error) {
			return types.Runner{}, errors.New("store down")
		}, false, "store down"},
		"wrong proof": {func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil }, true, "did not verify"},
	} {
		t.Run(name, func(t *testing.T) {
			hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: tc.lookup})
			defer hub.Close()
			org, run := runnerwire.Loopback()
			defer run.Close()
			done := make(chan error, 1)
			go func() { done <- hub.Serve(ctx, org, row.ID) }()
			if tc.sign {
				frame, err := run.ReadFrame(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var hello runnerwire.Hello
				if err := runnerwire.Unmarshal(frame.Payload, &hello); err != nil {
					t.Fatal(err)
				}
				id := row.ID.String()
				auth := runnerwire.Auth{RunnerID: id, Version: runnerwire.ProtocolVersion, Sig: runnerwire.SignAuth(wrong, hello.Nonce, id, hello.OrgURLSHA256)}
				if err := runnerwire.WriteControl(ctx, run, runnerwire.TypeAuth, auth); err != nil {
					t.Fatal(err)
				}
			}
			err := awaitValue(t, ctx, done)
			if !errors.Is(err, runnerwire.ErrAuth) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Serve = %v, want ErrAuth naming %q", err, tc.want)
			}
		})
	}
}
