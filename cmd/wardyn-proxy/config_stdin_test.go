// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

// TestReadStdinConfig (#1176): the config the Docker driver writes to stdin is
// read to EOF; an empty stream, or none within the timeout (a stopped proxy
// started again by hand), is an error, so the proxy exits non-zero rather than
// run without its policy.
func TestReadStdinConfig(t *testing.T) {
	if got, err := readStdinConfig(strings.NewReader(`{"run_id":"x"}`), time.Second); err != nil || string(got) != `{"run_id":"x"}` {
		t.Fatalf("readStdinConfig = %q, %v; want the config", got, err)
	}
	if _, err := readStdinConfig(strings.NewReader(" \n"), time.Second); err == nil {
		t.Error("an empty stdin was accepted as a config")
	}
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	if _, err := readStdinConfig(r, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "within") {
		t.Errorf("a stdin that never closes = %v; want a timeout error", err)
	}
}
