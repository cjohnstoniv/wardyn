// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "strings"

// The agent_image setup check, moved verbatim out of setup.go to keep that
// file under its frozen size cap (scripts/check-file-size.sh).

// agentImageCheck reports the resolved claude-code agent image so an operator
// sees, before a run ever fails, whether it is the Node-only convention image
// or a provisioned override — the readiness surface for the multi-toolchain image's
// BLOCKER-1 (a non-JS workspace exit-127s on the shipped default, silently).
// wardynd has no docker CLI (the compose build is distroless static) and no
// wired image-inspect capability on the Runner interface, so this is a NAME
// heuristic against the two known-Node-only convention refs, not a real
// `docker inspect` — labeled honestly as such rather than guessing further.
// Always info/warn, never fail: an operator-chosen image is assumed
// provisioned on purpose.
func agentImageCheck(images map[string]string) SetupCheck {
	ref := agentImage("claude-code", images)
	if isConventionLimitedToolchainImage(ref) {
		// Info, not warn. This is the SHIPPED DEFAULT: it is true of every stock
		// install, it is documented rather than misconfigured, and it clears only
		// by building or wiring a multi-toolchain image that a JS/Python operator
		// never needs. setup_checks.go reserves "info" for exactly that —
		// permanent or purely optional — and the first-run gate (ui setup-gate.ts)
		// redirects on warn, so grading this warn locked every stock install in
		// the funnel with no in-product way out.
		return SetupCheck{
			ID: "agent_image", Label: "Agent image toolchains", Status: "info",
			Detail: "The configured claude-code agent image (" + ref + ") is a shipped convention image with a " +
				"limited toolchain — a Go, Rust or Java workspace will fail verify/record with exit 127 " +
				"(toolchain not found).",
			Fix: "Wire a multi-toolchain image via WARDYN_AGENT_IMAGES (helm: env.WARDYN_AGENT_IMAGES) (e.g. build deploy/images/full " +
				"(the fat toolchain image), or your own image satisfying the IMAGE CONTRACT in deploy/images/README.md), or pass a " +
				"per-run base image in the New Run wizard's \"Sandbox image\" field — Wardyn wraps it with the runner tools.",
		}
	}
	// The setup connectivity probe (site_config_probe.go) dispatches the "base"
	// image, never "claude-code" (a probe is a bare curl task, not a coding
	// agent). The claude-code catalog row's ImageKey ALSO points at
	// base, so on a stock deployment these are the same image and stating them
	// as a contrast would present one image as two. They diverge only when an
	// operator pins claude-code in WARDYN_AGENT_IMAGES — which is exactly when
	// an operator needs to know the probe does not follow that pin.
	probeRef := agentImage("base", images)
	detail := "claude-code harness image: " + ref + ". "
	if probeRef != ref {
		detail += "The setup connectivity probe runs the `base` image instead: " + probeRef + ". "
	}
	return SetupCheck{
		ID: "agent_image", Label: "Agent image toolchains", Status: "info",
		Detail: detail + "Wardyn cannot inspect image contents from the " +
			"control plane (no docker CLI in the distroless build) — verify a workspace to confirm its toolchains.",
	}
}

// isConventionLimitedToolchainImage reports whether ref is one of Wardyn's own
// shipped convention images — the ones known, by construction, to carry a
// limited toolchain, so a Go/Rust/Java workspace fails verify/record at exit 127.
//
// agent-base is in this set: the claude-code catalog row's ImageKey points at
// `base` (agent-claude-code is not published), so the DEFAULT install resolves
// here and most needs the warning. Verified against
// ghcr.io/cjohnstoniv/agent-base:0.6.4: node, npm, python3 and git are present;
// go, java and cargo are not. The pre-rename :demo tag stays matched so holdout
// boxes keep the accurate warn.
func isConventionLimitedToolchainImage(ref string) bool {
	// Prefix, not an exact tag: the ghcr convention carries the daemon's own
	// version tag, not a fixed :latest — every published tag is the same
	// convention image.
	for _, p := range []string{
		"ghcr.io/cjohnstoniv/agent-claude-code:",
		"ghcr.io/cjohnstoniv/agent-base:",
	} {
		if strings.HasPrefix(ref, p) {
			return true
		}
	}
	switch ref {
	case "wardyn/agent-claude-code:local", "wardyn/agent-claude-code:demo", "wardyn/agent-base:local":
		return true
	}
	return false
}
