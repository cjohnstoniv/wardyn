// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The substrate a daemon runs (buildRunnerFromFlags) and the component selection /healthz
// reports. Split out of boot_deps.go for scripts/check-file-size.sh.

// substrateDeps is the registration Deps every substrate constructor receives,
// split out of buildRunnerFromFlags so a test can pin it (Record in particular)
// without registering a spy substrate in the process-global registry.
func substrateDeps(f *bootFlags, confRuntimes map[types.ConfinementClass]string, driveHostRoots []string) substrate.Deps {
	return substrate.Deps{
		ProxyImage:          *f.proxyImage,
		DriveProbeImage:     *f.driveProbeImage,
		ConfinementRuntimes: confRuntimes,
		UserDriveHostRoots:  driveHostRoots,
		// #1113: follow the resolved recording-store selection (WARDYN_RECORDING_STORE)
		// rather than letting the substrate hardcode Record — "off" must mean no
		// wardyn-rec wrap and no brokered:recording upload attempt, on every substrate.
		Record: substrate.RecordEnabled(*f.recordingSel),
	}
}

// buildRunnerFromFlags resolves the optional sandbox runner: "none" (nil runner,
// headless API-only) or any substrate registered in the substrate registry
// (internal/runner/substrate). Substrates SELF-REGISTER at init() like every
// other pluggable seam: the OCI/Docker substrate registers only under the
// "docker" build tag (parity rule: the control plane carries zero
// target-specific code by default), so a tagless `-runner docker` fails closed
// at resolve with a "not registered" error, and adding a substrate needs no
// edit here. Confinement substrate/runtime pins (pluggable CC3) are parsed
// fail-closed so a typo never silently downgrades isolation. Returns the
// orchestrator-wrapped runner (nil for "none") and the resolved runner target
// advertised on /healthz — reflect the ACTUAL resolved substrate, not a
// hardcoded "docker".
//
// refs is the durable ref->substrate RefStore (store.PG in production) wired
// into the orchestrator so lifecycle routing — and therefore the kill switch —
// survives a control-plane restart; nil keeps the in-memory-only behavior.
//
// driveHostRoots is the boot-parsed WARDYN_USER_DRIVE_HOST_ROOTS ceiling
// (parseMountCeilings), passed in rather than re-parsed here so the substrate's
// bind-time check and internal/api's authoring-time check are literally the
// same slice — and so the ceiling's boot warning is emitted once.
//
// The one exception to "the target IS the resolved substrate" is
// -runner-target (WARDYN_RUNNER_TARGET) with no runner configured: see the
// branch below. It cannot make this daemon dispatch — the runner stays nil —
// and it is ignored the moment a real substrate resolves.
func buildRunnerFromFlags(f *bootFlags, refs orchestrator.RefStore, driveHostRoots []string) (runner.Runner, string, error) {
	confRuntimes, err := parseConfinementMap(*f.confinementMap)
	if err != nil {
		return nil, "", err
	}
	if sel := *f.runnerSel; sel == "none" || sel == "" {
		slog.Info("wardynd: no runner selected; runs stay PENDING (headless API-only)")
		// WARDYN_RUNNER_TARGET, honoured ONLY here (bootFlags.runnerTargetOverride
		// says why it exists and why a configured runner ignores it), and failing
		// boot closed on an unknown value rather than advertising a target no
		// stored object could ever match.
		if t := strings.TrimSpace(*f.runnerTargetOverride); t != "" {
			if !slices.Contains(knownRunnerTargets(), t) {
				return nil, "", fmt.Errorf("unknown -runner-target %q (want one of %s): it names the substrate STORED objects validate against while -runner is \"none\", and is for test harnesses only",
					t, strings.Join(knownRunnerTargets(), ", "))
			}
			slog.Warn("wardynd: -runner-target overrides the advertised runner target while no runner is configured (test harness); objects may be REGISTERED for it, nothing is dispatched",
				slog.String("runner_target", t))
			return nil, t, nil
		}
		return nil, "none", nil
	}
	sub, err := substrate.New(*f.runnerSel, substrateDeps(f, confRuntimes, driveHostRoots))
	if err != nil {
		// Discriminate WHY substrate.New failed before printing the
		// same headline for both. A typo'd -runner or a substrate not compiled
		// into this build (e.g. "docker" without -tags docker) never reaches the
		// registry at all — Resolve's error is the right one for that. But a
		// REGISTERED substrate (e.g. "k8s") can still fail to CONSTRUCT — the
		// canary's flagship refuse-to-construct chief among them — and that
		// failure has nothing to do with -tags docker; printing the build-tag
		// headline over it sent every k8s boot refusal down the wrong
		// troubleshooting path.
		if !slices.Contains(substrate.Names(), *f.runnerSel) {
			return nil, "", fmt.Errorf("unknown -runner %q (want \"none\" or a registered substrate; the docker substrate requires a wardynd built with -tags docker): %w", *f.runnerSel, err)
		}
		return nil, "", fmt.Errorf("-runner %q failed to start: %w", *f.runnerSel, err)
	}
	slog.Info("wardynd: runner enabled", slog.String("substrate", sub.Name()), slog.String("proxy_image", *f.proxyImage))
	return orchestrator.New(sub).WithRefStore(refs), sub.Name(), nil
}

// knownRunnerTargets are the substrate names a STORED object may be registered
// for, derived from the drive backends rather than restated: a
// types.DriveBackend names exactly one target that can mount it, so this set is
// by construction the set -runner-target can usefully claim. Deriving it means
// a future backend for a new substrate widens the flag automatically instead of
// leaving a second list to forget. NOT substrate.Names(): that is what this
// build REGISTERED, and the harness this flag exists for is a tagless build
// whose registry is empty.
func knownRunnerTargets() []string {
	var out []string
	for _, b := range types.DriveBackends {
		if t := b.RunnerTarget(); t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// componentsInfo builds the pluggable-component selection advertised on
// /healthz. "selected" is the ACTUAL running impl; "available" is what each
// seam's registry has self-registered in THIS build (so a newly registered
// substrate/provider appears with no edit here — and a tagless build honestly
// shows sandbox.available=[]). Runtime facts only: the recommended-vs-shipped
// split lives in docs/PLUGGABILITY.md and ROADMAP.md, where a recommendation
// this build cannot yet run belongs. policy_engine has no registry yet, so it
// carries no "available".
//
// recStore is the ACTUAL constructed store (nil when disabled — e.g. the fs
// backend with no directory, the stock Helm install's default: persistence
// off, WARDYN_RECORDING_DIR empty). Without it, "recording" reported the
// *flag* (*f.recordingSel, e.g. "fs") regardless of whether that backend ever
// came up, so a stock deployment's /healthz claimed a live recording store
// while every run silently recorded nothing and the UI blamed "no session
// captured yet" — a broken promise, not a missing feature.
func componentsInfo(f *bootFlags, runnerTarget string, recStore recording.Store) map[string]api.ComponentInfo {
	sourceOf := func(selected, def string) string {
		if selected == def {
			return "default"
		}
		return "configured"
	}
	recInfo := api.ComponentInfo{Selected: *f.recordingSel, Available: recording.Names(), Source: sourceOf(*f.recordingSel, "pg")}
	if recStore == nil {
		recInfo = api.ComponentInfo{Selected: "none", Available: recording.Names(), Source: "disabled"}
	}
	return map[string]api.ComponentInfo{
		"identity":      {Selected: *f.identitySel, Available: identity.Names(), Source: sourceOf(*f.identitySel, "embedded")},
		"secret_store":  {Selected: *f.secretStoreSel, Available: secretstore.Names(), Source: sourceOf(*f.secretStoreSel, "pg")},
		"recording":     recInfo,
		"policy_engine": {Selected: "builtin"},
		"sandbox":       {Selected: runnerTarget, Available: substrate.Names(), Source: sourceOf(*f.runnerSel, "none")},
	}
}
