// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"github.com/google/uuid"
)

// recorderArgv builds the argv the agent container runs when session recording
// is enabled. It delegates to wardyn-rec (a thin binary inside the agent
// image) so the GPL recorder (asciinema) is exec'd as a subprocess, never
// linked into Wardyn (license hygiene). When recording is disabled, the raw
// agent argv is returned unchanged.
//
// Layout:
//
//	wardyn-rec -cast-dir <dir> [-out-dir <mount>] [-upload-url <proxy route>] -run <id> -- <agent argv...>
//
// wardyn-rec decides at runtime whether asciinema is present; the driver does
// not need to know. uploadURL is the DEFAULT delivery path: the proxy's
// brokered recording route (PUT /wardyn/v1/recordings/{run}), which injects
// the run token and lets the control plane MASK secrets before persisting the
// cast (secret masking lives control-plane-side; the registry of secret values
// is never in the sandbox). outDir (RecordingMountTarget) is the optional
// shared-mount fallback and carries TWO documented limitations: (1) all agent
// containers run the same uid, so a shared mount has NO cross-run isolation; and
// (2) it bypasses the control plane, so the cast it writes is UNMASKED — secret
// masking is structurally impossible here (wardyn-rec holds no secret values, by
// design). Use the brokered upload path where recordings are viewer-exposed.
//
// HIGH-finding hardening: the masked upload path and the unmasked shared-mount
// -out-dir are MUTUALLY EXCLUSIVE. When an uploadURL is configured we drop the
// shared-mount -out-dir entirely so wardyn-rec can NEVER also drop an UNMASKED
// <runID>.cast into the API-served replay store (cross-run-writable, viewer-
// exposed). The shared mount is only ever used as the reduced-isolation
// FALLBACK when no control-plane upload path exists.
func recorderArgv(recBinary, castDir, outDir, uploadURL string, runID uuid.UUID, agentArgv []string, record bool) []string {
	if !record || recBinary == "" {
		return agentArgv
	}
	out := []string{
		recBinary,
		"-cast-dir", castDir,
	}
	// Prefer the masked control-plane upload over the unmasked shared mount: if
	// both are offered, suppress -out-dir so no unmasked cast reaches a path the
	// API serves. -out-dir is only emitted as the fallback (uploadURL == "").
	if uploadURL != "" {
		out = append(out, "-upload-url", uploadURL)
	} else if outDir != "" {
		out = append(out, "-out-dir", outDir)
	}
	out = append(out, "-run", runID.String(), "--")
	return append(out, agentArgv...)
}
