// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// The peer check has its own bound, so a slow lsof reads as a refusal that names the bound, well inside the
// sign-in's budget.
func TestLsofRootHoldsHasItsOwnBound(t *testing.T) {
	defer func(old time.Duration) { lsofBound = old }(lsofBound)
	lsofBound = 20 * time.Millisecond
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		if c, err := l.Accept(); err == nil {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hang := runnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // frees the hanging runner too if the production bound is ever removed
	done := make(chan error, 1)
	go func() { done <- lsofRootHolds(ctx, hang, c) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "took longer than") {
			t.Fatalf("err = %v, want a refusal naming the bound", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the peer check did not end at its bound")
	}
}

type runnerFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}
