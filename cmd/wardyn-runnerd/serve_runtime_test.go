// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func TestRevokedWireKeepsContainmentAliveWithoutReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	latched, retried := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- runRunnerServices(ctx,
			func(context.Context) error {
				calls.Add(1)
				close(latched)
				return errors.Join(runnerwire.ErrRevoked, errors.New("transient stop failure"))
			},
			func(life context.Context) error {
				<-latched
				retry := time.NewTimer(20 * time.Millisecond)
				defer retry.Stop()
				select {
				case <-life.Done():
					return errors.New("revocation canceled unresolved containment")
				case <-retry.C:
				}
				close(retried)
				<-life.Done()
				return life.Err()
			})
	}()
	select {
	case <-retried:
	case err := <-done:
		t.Fatalf("stopped before retry: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-done:
		t.Fatalf("revocation stopped containment: %v", err)
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("daemon cancellation did not stop both loops")
	}
	if calls.Load() != 1 {
		t.Fatalf("revoked identity reconnected %d times", calls.Load())
	}
}

func TestContainmentFailureCancelsConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	failure := errors.New("containment state corrupted")
	stopped := make(chan struct{})
	err := runRunnerServices(ctx,
		func(ctx context.Context) error { <-ctx.Done(); close(stopped); return ctx.Err() },
		func(context.Context) error { return failure })
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("connection survived containment failure")
	}
}
