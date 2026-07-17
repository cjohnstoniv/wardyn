// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"log/slog"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newDockerRunner constructs the Docker driver. Compiled only with `-tags docker`
// so the default control-plane build carries zero target-specific code (parity
// rule). The docker client transitive dep must resolve for this build.
//
// Record is enabled so Exec wraps the agent argv with wardyn-rec (PTY session
// recording). WARDYN_RECORDING_MOUNT names a Docker volume (or absolute host
// path) shared between agent containers and wardynd's -recording-dir; when
// set, wardyn-rec delivers finished casts there (-out-dir) and replay lights
// up. Single-host delivery only — multi-node upload (via proxy) lands in v0.5.
//
// SECURITY (HIGH-finding): WARDYN_RECORDING_MOUNT is the REDUCED-ISOLATION
// fallback delivery path. Casts written to the shared mount are UNMASKED (secret
// masking lives control-plane-side, on the brokered upload path) and the mount
// has NO cross-run isolation (all agent containers share one uid). The driver
// now prefers the masked brokered upload whenever a run token exists and only
// uses the shared mount when no upload path is available — but operators who set
// this must understand the tradeoff, so we warn loudly at startup.
func newDockerRunner(proxyImage string, confRuntimes map[types.ConfinementClass]string) (runner.Runner, error) {
	recordingMount := os.Getenv("WARDYN_RECORDING_MOUNT")
	if recordingMount != "" {
		slog.Warn("wardynd: WARDYN_RECORDING_MOUNT is set — this is the reduced-isolation recording fallback: casts delivered via the shared mount are UNMASKED and have NO cross-run isolation. The masked brokered upload path is preferred whenever available; prefer leaving WARDYN_RECORDING_MOUNT unset for viewer-exposed recordings.",
			slog.String("recording_mount", recordingMount),
		)
	}
	sub, err := docker.New(docker.Config{
		ProxyImage:          proxyImage,
		Record:              true,
		RecordingMount:      recordingMount,
		ConfinementRuntimes: confRuntimes,
	})
	if err != nil {
		return nil, err
	}
	// The orchestrator presents the runner.Runner surface and multiplexes
	// confinement substrates; today that is the single OCI/Docker substrate.
	return orchestrator.New(sub), nil
}
