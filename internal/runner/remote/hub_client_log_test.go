// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
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

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The runner logs why the org sent it away, then logs reaching READY once the
// org admits it.
func TestRunnerLogsGoawayReasonThenReady(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	var serving sync.WaitGroup
	defer func() { cancel(); serving.Wait() }()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"),
		State: types.RunnerUnclaimed, CreatedAt: time.Now()}
	var claimed atomic.Bool
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example",
		Lookup: func(context.Context, uuid.UUID) (types.Runner, error) {
			current := row
			if claimed.Load() {
				current.State = types.RunnerClaimed
			}
			return current, nil
		},
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	})
	defer hub.Close()
	online, resumed := make(chan *runnerwire.Peer, 1), make(chan bool, 1)
	// Each dial waits for the test to release it, so the stored row changes
	// only between sessions.
	dials, release := make(chan struct{}), make(chan struct{})
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key,
		Hooks: sessionHooks(online, resumed),
		Dial: func(ctx context.Context) (runnerwire.Conn, error) {
			select {
			case dials <- struct{}{}:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			org, run := runnerwire.Loopback()
			serving.Go(func() { _ = hub.Serve(ctx, org, row.ID) })
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	_ = awaitValue(t, ctx, dials)
	release <- struct{}{}
	// Run logs a failure before it waits to redial.
	_ = awaitValue(t, ctx, dials)
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "runner_unclaimed") || !strings.Contains(out, "retry_in=") {
		t.Fatalf("GOAWAY was not logged with its reason and retry delay:\n%s", out)
	}
	claimed.Store(true)
	release <- struct{}{}
	if awaitValue(t, ctx, resumed) {
		t.Fatal("first admitted session resumed")
	}
	_ = awaitValue(t, ctx, online)
	if out := logs.String(); strings.Count(out, `level=INFO msg="runner connected to the organisation"`) != 1 {
		t.Fatalf("READY was not logged once:\n%s", out)
	}
	cancel()
	_ = awaitValue(t, context.Background(), done)
}

// A runner whose registration changed while it authenticated is told so, in a GOAWAY, rather than
// met with a bare close it can only retry.
func TestHubTellsARunnerWhoseRegistrationChanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"),
		State: types.RunnerClaimed, CreatedAt: time.Now()}
	reads := 0
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example",
		Lookup: func(context.Context, uuid.UUID) (types.Runner, error) {
			reads++
			current := row
			if reads > 1 {
				current.PublicKey = ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)) // re-registered under another key
			}
			return current, nil
		},
	})
	defer hub.Close()
	org, run := runnerwire.Loopback()
	go func() { _ = hub.Serve(ctx, org, row.ID) }()
	_, err = runnerwire.AuthenticateOrg(ctx, run, row.ID.String(), "https://org.example", key, nil)
	if !errors.Is(err, runnerwire.ErrGoaway) || !strings.Contains(err.Error(), runnerwire.ReasonRegistrationLapsed) {
		t.Fatalf("the runner was told %v, want a GOAWAY naming %s", err, runnerwire.ReasonRegistrationLapsed)
	}
}
