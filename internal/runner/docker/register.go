// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"log/slog"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// Self-register the OCI/Docker substrate so a blank import (cmd/wardynd under
// `-tags docker`) makes "docker" selectable via -runner/WARDYN_RUNNER. Like every
// file in this package the registration is compiled ONLY under the docker build
// tag: a tagless wardynd has no "docker" registration, so `-runner docker` fails
// closed at registry resolve ("not registered") — the same class of error as the
// old hardcoded not-compiled-in branch, with zero target-specific code in the
// default control-plane build (the parity rule).
//
// Record is enabled so Exec wraps the agent argv with wardyn-rec (PTY session
// recording). WARDYN_RECORDING_MOUNT names a Docker volume (or absolute host
// path) shared between agent containers and wardynd's -recording-dir; when set,
// wardyn-rec delivers finished casts there (-out-dir) and replay lights up.
// Single-host delivery only — multi-node upload (via proxy) lands in v0.5.
//
// SECURITY (HIGH-finding): WARDYN_RECORDING_MOUNT is the REDUCED-ISOLATION
// fallback delivery path. Casts written to the shared mount are UNMASKED (secret
// masking lives control-plane-side, on the brokered upload path) and the mount
// has NO cross-run isolation (all agent containers share one uid). The driver
// prefers the masked brokered upload whenever a run token exists and only uses
// the shared mount when no upload path is available — but operators who set this
// must understand the tradeoff, so we warn loudly at startup.
func init() {
	substrate.Register("docker", func(d substrate.Deps) (substrate.Substrate, error) {
		recordingMount := os.Getenv("WARDYN_RECORDING_MOUNT")
		if recordingMount != "" {
			slog.Warn("wardynd: WARDYN_RECORDING_MOUNT is set — this is the reduced-isolation recording fallback: casts delivered via the shared mount are UNMASKED and have NO cross-run isolation. The masked brokered upload path is preferred whenever available; prefer leaving WARDYN_RECORDING_MOUNT unset for viewer-exposed recordings.",
				slog.String("recording_mount", recordingMount),
			)
		}
		s, err := New(Config{
			ProxyImage:          d.ProxyImage,
			Record:              true,
			RecordingMount:      recordingMount,
			ConfinementRuntimes: d.ConfinementRuntimes,
		})
		if err != nil {
			return nil, err
		}
		return s, nil // avoid the typed-nil interface trap
	})
}
