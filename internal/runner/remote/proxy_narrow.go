// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

var _ runner.ProxyNarrower = (*Substrate)(nil)

// errCapsUnknown: caps are held in memory only, so after wardynd restarts nothing says whether the runner
// narrows until it reports them again.
var errCapsUnknown = errors.New("remote: the runner has not reported its capabilities since this server started")

// ProxyNarrowState reads ref's proxy deny list. A runner without ProxyNarrow answers ErrNarrowNotOffered with
// no call, so the edit takes the revive path; one whose caps are not known yet answers ErrNarrowNotSent, so
// the edit is tried again rather than refused as if the runner could not narrow.
func (s *Substrate) ProxyNarrowState(ctx context.Context, ref string) (runner.ProxyNarrowState, error) {
	return s.narrow(ctx, ref, nil, runner.ErrNarrowNotOffered)
}

// NarrowProxy adds denies to ref's proxy deny list. A runner that lost ProxyNarrow since the probe answers
// ErrNarrowNotSent with no call.
func (s *Substrate) NarrowProxy(ctx context.Context, ref string, denies []string) (runner.ProxyNarrowState, error) {
	return s.narrow(ctx, ref, denies, runner.ErrNarrowNotSent)
}

// narrow sends narrow_proxy. Only an answer that proves nothing reached the proxy is definitive: a call lost
// after it was sent may have been applied (runnerwire.Peer.Call cannot tell), so it is ErrNarrowUncertain.
func (s *Substrate) narrow(ctx context.Context, ref string, deny []string, noCap error) (runner.ProxyNarrowState, error) {
	c, known := s.Caps()
	if !known {
		return runner.ProxyNarrowState{}, fmt.Errorf("%w: %w", runner.ErrNarrowNotSent, errCapsUnknown)
	}
	if !c.ProxyNarrow {
		return runner.ProxyNarrowState{}, noCap
	}
	if err := s.own(ref); err != nil {
		return runner.ProxyNarrowState{}, fmt.Errorf("%w: %w", runner.ErrNarrowNotSent, err)
	}
	if !s.t.Online() {
		return runner.ProxyNarrowState{}, fmt.Errorf("%w: %w", runner.ErrNarrowNotSent, errOffline)
	}
	var res runnerwire.NarrowProxyResult
	err := s.call(ctx, runnerwire.MethodNarrowProxy, runnerwire.NarrowProxyArgs{Ref: ref, Deny: deny}, &res)
	switch {
	case err == nil:
		return runner.ProxyNarrowState{ConfigSHA256: res.ConfigSHA256, BaseSHA256: res.BaseSHA256,
			LiveDenies: res.LiveDenies, Changed: res.Changed, BaseChecked: res.BaseChecked}, nil
	case errors.Is(err, runner.ErrNarrowUnsupported), errors.Is(err, runner.ErrNarrowRefused),
		errors.Is(err, runner.ErrNarrowNotListening), errors.Is(err, runner.ErrNarrowNotSent),
		errors.Is(err, runner.ErrNarrowUncertain):
		return runner.ProxyNarrowState{}, err
	}
	return runner.ProxyNarrowState{}, fmt.Errorf("%w: %w", runner.ErrNarrowUncertain, err)
}
