// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// exitCodeFor is the process-exit taxonomy CI branches on. These pin each
// branch: a run outcome (*exitError) wins over everything, then API errors map
// by status class, then a transport (*url.Error) failure, then the catch-all.

func TestExitCodeFor_ClientErrorIs3(t *testing.T) {
	err := &sdk.APIError{Status: 404}
	if got := exitCodeFor(err); got != 3 {
		t.Errorf("exitCodeFor(404) = %d, want 3", got)
	}
	// A wrapped 4xx must still resolve through errors.As.
	if got := exitCodeFor(fmt.Errorf("poll: %w", err)); got != 3 {
		t.Errorf("exitCodeFor(wrapped 404) = %d, want 3", got)
	}
}

func TestExitCodeFor_ServerErrorIs4(t *testing.T) {
	err := &sdk.APIError{Status: 503}
	if got := exitCodeFor(err); got != 4 {
		t.Errorf("exitCodeFor(503) = %d, want 4", got)
	}
}

func TestExitCodeFor_AuthIs2(t *testing.T) {
	for _, code := range []int{401, 403} {
		err := &sdk.APIError{Status: code}
		if got := exitCodeFor(err); got != 2 {
			t.Errorf("exitCodeFor(%d) = %d, want 2", code, got)
		}
	}
}

func TestExitCodeFor_NetworkUnreachableIs5(t *testing.T) {
	// A real refused connection: the client's transport returns a *url.Error,
	// which do() wraps — exitCodeFor must still see it via errors.As. Port 1 on
	// loopback refuses immediately (no timeout wait).
	c := &sdk.Client{BaseURL: "http://127.0.0.1:1"}
	_, err := c.ListRuns(context.Background())
	if err == nil {
		t.Fatal("expected a transport error dialing 127.0.0.1:1, got nil")
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want a *url.Error in the chain", err)
	}
	if got := exitCodeFor(err); got != 5 {
		t.Errorf("exitCodeFor(transport error) = %d, want 5", got)
	}
}

func TestExitCodeFor_ExitErrorWins(t *testing.T) {
	// An *exitError wrapping an API 500 must yield the exitError's code, NOT the
	// apiError's server-class 4 — the run outcome from --wait always wins.
	err := &exitError{code: 42, err: &sdk.APIError{Status: 500}}
	if got := exitCodeFor(err); got != 42 {
		t.Errorf("exitCodeFor(exitError over apiError) = %d, want 42 (exitError wins)", got)
	}
}

func TestExitCodeFor_UnknownIs1(t *testing.T) {
	if got := exitCodeFor(errors.New("something else entirely")); got != 1 {
		t.Errorf("exitCodeFor(plain error) = %d, want 1", got)
	}
}
