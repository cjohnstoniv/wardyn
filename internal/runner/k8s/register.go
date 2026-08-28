// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"os"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// Self-register the Kubernetes substrate so a blank import (cmd/wardynd under
// `-tags k8s`) makes "k8s" selectable via -runner/WARDYN_RUNNER. Compiled ONLY
// under the k8s tag, so a tagless or -tags docker wardynd fails `-runner k8s`
// closed at registry resolve ("not registered") and carries no k8s-specific
// code — mirrors docker/register.go exactly.
func init() {
	substrate.Register("k8s", func(d substrate.Deps) (substrate.Substrate, error) {
		s, err := New(Config{
			Namespace:             resolveNamespace(os.Getenv("WARDYN_K8S_NAMESPACE")),
			ProxyImage:            d.ProxyImage,
			ImagePullSecret:       os.Getenv("WARDYN_K8S_IMAGE_PULL_SECRET"),
			ConfinementRuntimes:   d.ConfinementRuntimes,
			AllowUnenforcedNetPol: os.Getenv("WARDYN_K8S_ALLOW_UNENFORCED_NETPOL") == "1",
			AckAmbientDefaultDeny: os.Getenv("WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY") == "1",
		})
		if err != nil {
			return nil, err
		}
		return s, nil // avoid the typed-nil interface trap
	})
}
