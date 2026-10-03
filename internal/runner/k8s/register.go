// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"os"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// buildConfig maps registration Deps to the k8s driver's Config. Extracted to
// a pure function so its Record wiring is testable without a live cluster
// (register_test.go's TestBuildConfig_RecordFollowsDeps pins it): Deps.Record
// — itself derived from the boot recording-store selection, see
// substrate.RecordEnabled and cmd/wardynd's buildRunnerFromFlags — must reach
// Config.Record verbatim. #1113 was exactly this wiring hardcoding
// `Record: true` here regardless of WARDYN_RECORDING_STORE, so an install
// with recording off still wrapped every exec in wardyn-rec and still logged
// a brokered:recording deny for a feature that was switched off.
func buildConfig(d substrate.Deps) Config {
	return Config{
		Namespace:           resolveNamespace(os.Getenv("WARDYN_K8S_NAMESPACE")),
		ProxyImage:          d.ProxyImage,
		ImagePullSecret:     os.Getenv("WARDYN_K8S_IMAGE_PULL_SECRET"),
		SandboxPlacement:    os.Getenv("WARDYN_K8S_SANDBOX_PLACEMENT"),
		ReadNodes:           cliutil.EnvBool("WARDYN_K8S_READ_NODES", false),
		Record:              d.Record,
		ConfinementRuntimes: d.ConfinementRuntimes,
		// cliutil.EnvBool, not a literal "1" compare (#202): the shared
		// 1/true/yes/on token set, and a garbage value exits 2 at boot
		// instead of silently staying off.
		AllowUnenforcedNetPol: cliutil.EnvBool("WARDYN_K8S_ALLOW_UNENFORCED_NETPOL", false),
		AckAmbientDefaultDeny: cliutil.EnvBool("WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY", false),
	}
}

// Self-register the Kubernetes substrate so a blank import (cmd/wardynd under
// `-tags k8s`) makes "k8s" selectable via -runner/WARDYN_RUNNER. Compiled ONLY
// under the k8s tag, so a tagless or -tags docker wardynd fails `-runner k8s`
// closed at registry resolve ("not registered") and carries no k8s-specific
// code — mirrors docker/register.go exactly.
func init() {
	substrate.Register("k8s", func(d substrate.Deps) (substrate.Substrate, error) {
		s, err := New(buildConfig(d))
		if err != nil {
			return nil, err
		}
		return s, nil // avoid the typed-nil interface trap
	})
}
