// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestAttachViaLocalRefusedOnWindows: the Windows build has no local transport, and --via local says so
// before any call reaches the organisation.
func TestAttachViaLocalRefusedOnWindows(t *testing.T) {
	if localAttachOS {
		t.Fatal("localAttachOS is true on Windows")
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	err := runAttach(context.Background(), &sdk.Client{BaseURL: srv.URL}, "7f3c9a41-b2d8-4e6a-9c15-3d0b8e2f4a71", attachOptions{via: viaLocal})
	if err == nil || err.Error() != localAttachWindowsRefusal {
		t.Fatalf("attach --via local = %v, want %q", err, localAttachWindowsRefusal)
	}
	if calls.Load() != 0 {
		t.Errorf("%d calls reached the organisation", calls.Load())
	}
}
