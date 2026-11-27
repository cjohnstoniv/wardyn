// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// serverDriver is the k8s substrate in server mode with keep off: the Driver plus a new proxy for a run that
// StopLostProxyAt kept after it was lost, behind the run's same Service. Every other run, a running one above all,
// is refused with runner.ErrReviveUnsupported before anything is written (lostReplaceGate), as the Driver alone
// would answer. Its StopLostProxyAt is the Driver's.
type serverDriver struct{ *Driver }

var (
	_ runner.ProxyReviverAt   = (*serverDriver)(nil)
	_ runner.LostProxyStopper = (*serverDriver)(nil)
)

// CanReplaceProxy answers nil only for a run that StopLostProxyAt kept.
func (s *serverDriver) CanReplaceProxy(ctx context.Context, ref string) error {
	_, _, _, err := s.lostReplaceGate(ctx, ref)
	return err
}

// the cluster pulls the proxy image per its pull policy; nothing to stage
func (s *serverDriver) EnsureProxyImage(context.Context) error { return nil }

// ReplaceProxy is ReplaceProxyHashedAt for no generation.
func (s *serverDriver) ReplaceProxy(ctx context.Context, ref string, cfgJSON []byte) error {
	_, err := s.ReplaceProxyHashedAt(ctx, ref, cfgJSON, 0)
	return err
}

// ReplaceProxyHashed is ReplaceProxyHashedAt for no generation.
func (s *serverDriver) ReplaceProxyHashed(ctx context.Context, ref string, cfgJSON []byte) (string, error) {
	return s.ReplaceProxyHashedAt(ctx, ref, cfgJSON, 0)
}

// ReplaceProxyHashedAt starts a new proxy behind the run's Service for a run that StopLostProxyAt kept, acting for
// policy generation gen (runner.ProxyReviverAt), as the keep backend does (replaceProxyAt). The new proxy pod has no
// owner, as every proxy pod, and the agent stays with none.
func (s *serverDriver) ReplaceProxyHashedAt(ctx context.Context, ref string, cfgJSON []byte, gen int) (string, error) {
	runID, ns, svc, err := s.lostReplaceGate(ctx, ref)
	if err != nil {
		return "", err
	}
	return s.replaceProxyAt(ctx, ns, runID, svc, cfgJSON, gen, nil)
}
