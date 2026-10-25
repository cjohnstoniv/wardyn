// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// SetupCodeFile, in the login's VM directory (Layout.Root), holds the
// one-time code while macOS waits for its setup by hand: the account name and
// the code the Mac's person types as its password in Setup Assistant. It is
// read only on the Mac; no log, event or status output carries the code, and
// status names this file instead.
const SetupCodeFile = "setup-code.json"

// SetupCode is the setup-code file's path.
func (l Layout) SetupCode() string { return filepath.Join(l.Root, SetupCodeFile) }

// SetupCodeRecord is the setup-code file's content.
type SetupCodeRecord struct {
	Account string `json:"account"`
	Code    string `json:"code"`
}

// setupByHand writes a fresh one-time code to the setup-code file, emits
// setup_by_hand with no detail, opens the guest's window with screen (nil:
// none, as in a build without AppKit) and waits for the guest's Remote Login,
// closing the window when the wait ends. A window that cannot open stops
// prepare: nobody could finish the setup. It returns the code, for stage 1's
// first login.
func (p *prep) setupByHand(ctx context.Context, nw PrepNetwork, screen func() (func(), error)) (string, error) {
	code, err := randomCode(8)
	if err != nil {
		return "", err
	}
	if err := writeSetupCode(p.Layout.SetupCode(), SetupCodeRecord{Account: provisionUser, Code: string(code)}); err != nil {
		return "", err
	}
	p.emit(PrepareEvent{Phase: PhaseSetupByHand})
	if screen != nil {
		closeWindow, err := screen()
		if err != nil {
			return "", fmt.Errorf("macvm: setup by hand needs the guest's window on this Mac: %w", err)
		}
		defer closeWindow()
	}
	if err := waitPort(ctx, nw, 22, p.SetupByHandWait); err != nil {
		return "", err
	}
	return string(code), nil
}

// writeSetupCode creates path new (O_EXCL|O_NOFOLLOW, mode 0600) in its 0700
// directory; a leftover from an earlier prepare is removed first, so the
// exclusive create never writes through anything planted there.
func writeSetupCode(path string, r SetupCodeRecord) error {
	if err := mkdirPrivate(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

// removeSetupCode deletes the setup-code file: when stage 1 has replaced the
// code, when prepare discards the base, and when the prepare that wrote it
// ends.
func (p *prep) removeSetupCode() {
	if err := os.Remove(p.Layout.SetupCode()); err != nil && !os.IsNotExist(err) {
		p.Log.Warn("macvm: the setup-code file was not deleted", "path", p.Layout.SetupCode(), "err", err.Error())
	}
}
