// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// errUpdateToolsBusy and errUpdateToolsNoBase are update-tools' own refusals.
var (
	errUpdateToolsBusy   = errors.New("a prepare, tools update or edit-base of this Mac's macOS base is running; run update-tools when it ends")
	errUpdateToolsNoBase = errors.New("this Mac has no ready macOS base to update; run `wardyn-runnerd macos-vm prepare` first, or wait for the prepare that is running")
)

// updateTools is `macos-vm update-tools`: the same update, asked for by the operator. It passes --force,
// so the helper updates a base that looks current and retries every withheld harness. Like editBase, it
// refuses while a prepare, tools update or edit of this runner's runs, so it never queues behind the lock.
func (m *macOSVMs) updateTools(ctx context.Context) error {
	if m.ledger == nil {
		return errors.New(cmp.Or(m.refusal, m.why))
	}
	m.reconcilePrepare()
	for _, k := range []admission.Kind{admission.KindPrepare, admission.KindToolsUpdate, kindEditBase} {
		if slices.ContainsFunc(m.own(k), func(e admission.Entry) bool { return !m.vmGone(e) }) {
			return errUpdateToolsBusy
		}
	}
	man, err := macvm.ReadyBase(m.layout.BaseDir(0))
	if err != nil {
		return errUpdateToolsNoBase
	}
	return m.startToolsUpdateWith(ctx, man, true)
}
