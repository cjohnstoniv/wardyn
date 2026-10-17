// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2etmux && !docker && linux

package main

// Test-only: registers the local-tmux substrate (internal/runner/localtmux) so
// the chromium e2e gate can attach the production console to a real tmux. The
// release build never sets e2etmux.
import _ "github.com/cjohnstoniv/wardyn/internal/runner/localtmux"
