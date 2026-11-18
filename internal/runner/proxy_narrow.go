// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
)

// ProxyNarrowState is what a running proxy reports about its live deny list.
type ProxyNarrowState struct {
	ConfigSHA256 string   // proxy.ConfigSHA256 of the config it enforces now
	BaseSHA256   string   // proxy.LiveDenyBaseSHA256 of the config it was started with
	LiveDenies   []string // the list it enforces now, normalized
	Changed      bool     // NarrowProxy only: the call added a host
	BaseChecked  bool     // runner mode: the runner matched BaseSHA256 to the base it recorded at create
}

// ProxyNarrower narrows a live sandbox's proxy in place: hosts join its deny list, which never shrinks.
// A substrate that offers it lets a live egress edit skip the proxy replacement.
type ProxyNarrower interface {
	// ProxyNarrowState reads the proxy's list without changing it.
	ProxyNarrowState(ctx context.Context, ref string) (ProxyNarrowState, error)
	// NarrowProxy adds denies to the proxy's list and answers the state it holds afterwards.
	NarrowProxy(ctx context.Context, ref string, denies []string) (ProxyNarrowState, error)
}

var (
	ErrNarrowNotOffered   = errors.New("runner: this substrate does not narrow a proxy in place")
	ErrNarrowUnsupported  = errors.New("runner: this sandbox's proxy cannot be narrowed in place")
	ErrNarrowRefused      = errors.New("runner: the proxy refused the narrowing and changed nothing")
	ErrNarrowNotListening = errors.New("runner: the proxy is not taking narrowings now, so nothing was delivered")
	ErrNarrowNotSent      = errors.New("runner: nothing reached the proxy, so nothing changed")
	ErrNarrowUncertain    = errors.New("runner: the narrowing may have reached the proxy")
)
