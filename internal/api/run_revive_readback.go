// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reviveReadbackSupported refuses, before any change, a revive whose new proxy must confirm its policy
// (an expected start, run_proxy_readback.go) on a substrate that cannot answer the hash of the bytes it
// hands that proxy (no runner.ProxyReviverHashed).
func (s *Server) reviveReadbackSupported(ctx context.Context, run types.AgentRun, rv runner.ProxyReviver) *reviveError {
	if _, ok := s.cfg.Store.(store.RunProxyStarts); !ok {
		return nil
	}
	if _, ok := rv.(runner.ProxyReviverHashed); ok || s.proxyStartMode(ctx, run) == store.ProxyStartLegacy {
		return nil
	}
	return reviveRefused(http.StatusConflict, reasonReviveReadbackUnsupported,
		"this run's substrate cannot confirm a new proxy's policy, so its proxy cannot be replaced")
}

// replaceRevivedProxyHashed is replaceRevivedProxy through runner.ProxyReviverHashed when rv has it,
// answering the hash of the bytes the new proxy was handed; "" from a reviver without it.
func (s *Server) replaceRevivedProxyHashed(ctx context.Context, rv runner.ProxyReviver, run types.AgentRun, cfgJSON []byte) (string, error) {
	h, ok := rv.(runner.ProxyReviverHashed)
	if !ok {
		return "", s.replaceRevivedProxy(ctx, rv, run, cfgJSON)
	}
	if run.RunnerID != nil {
		if err := s.renewRunnerLease(ctx, run, leaseWait); err != nil {
			return "", err
		}
	}
	return h.ReplaceProxyHashed(ctx, run.SandboxRef, cfgJSON)
}
