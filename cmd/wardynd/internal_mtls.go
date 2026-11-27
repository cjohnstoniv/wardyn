// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/svidsource"
)

// loadInternalMTLS is WARDYN_INTERNAL_MTLS, read once at boot. Off, it is nil: the internal listener and
// internalAuth are as before. On, it refuses to start unless the internal listener binds (an https
// control-plane URL), the identity provider holds a SPIRE source whose bundle verifies run certificates
// (spire), and WARDYN_RUNNER is k8s, the only runner whose proxies get a run certificate in this release.
// It reads runnerSel, the value refuseSpireOffKubernetes reads, never the runner target a test harness can
// set on its own.
func loadInternalMTLS(on bool, prov identity.Provider, controlURL, runnerSel string) (*api.InternalMTLS, error) {
	if !on {
		return nil, nil
	}
	if !internalListenBinds(controlURL) {
		return nil, errors.New("refusing to start: WARDYN_INTERNAL_MTLS needs an https WARDYN_CONTROL_PLANE_URL, so that the internal TLS listener runs")
	}
	h, ok := prov.(svidsource.Holder)
	if !ok || h.SVIDSource() == nil {
		return nil, errors.New("refusing to start: WARDYN_INTERNAL_MTLS needs WARDYN_IDENTITY=spire, whose trust bundle verifies each run's certificate")
	}
	if runnerSel != "k8s" {
		return nil, fmt.Errorf("refusing to start: WARDYN_INTERNAL_MTLS needs WARDYN_RUNNER=k8s, the only runner whose proxies hold a run certificate (WARDYN_RUNNER is %q)", runnerSel)
	}
	src := h.SVIDSource()
	// The provider's own SVID is in the trust domain it mints run IDs in.
	own, err := src.GetX509SVID()
	if err != nil {
		return nil, fmt.Errorf("refusing to start: WARDYN_INTERNAL_MTLS: read the identity provider's trust domain: %w", err)
	}
	slog.Info("wardynd: proxy-facing listener requires each run's certificate")
	return &api.InternalMTLS{Bundles: src, TrustDomain: own.ID.TrustDomain().Name()}, nil
}
