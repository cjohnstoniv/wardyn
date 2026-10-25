// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
)

// DesktopProbe is the helper's own RFB client for stage 2's control check:
// it logs in to the guest's Screen Sharing with VncAuth over the desktop
// vsock path and moves the pointer to (x, y).
type DesktopProbe interface {
	MovePointer(ctx context.Context, password []byte, x, y int) error
}

// ErrDesktopPassword fails stage 2 when the guest did not take a Screen
// Sharing password. The base is then never published ready: observe is the
// answer to a pointer that did not move, not to a password that was not set.
var ErrDesktopPassword = errors.New("macvm: the macOS guest did not take its Screen Sharing password")

// The control check's set point: the display's centre, the same point in
// RFB's top-left and the guest's bottom-left pointer coordinates.
const (
	controlCheckX = DisplayWidth / 2
	controlCheckY = DisplayHeight / 2
)

// controlCheck is stage 2's first step on a prepare. It sets a fresh
// per-boot password through tools mode's desktop_password step, logs in with
// it and moves the pointer, and reads the pointer back in the console
// session: a match is DesktopControl, anything else DesktopObserve. It then
// sets a second fresh password and discards both, so the base keeps a VNC
// password no process holds. A password the guest did not take fails the
// check (ErrDesktopPassword), whatever the pointer did.
func controlCheck(ctx context.Context, g ToolsGuest, probe DesktopProbe) (string, error) {
	pw, err := vncPassword()
	if err != nil {
		return "", err
	}
	defer clear(pw)
	if err := setDesktopPassword(ctx, g, pw, "the control check's"); err != nil {
		return "", err
	}
	desktop := DesktopObserve
	if probe != nil && probe.MovePointer(ctx, pw, controlCheckX, controlCheckY) == nil {
		if x, y, err := g.Pointer(ctx); err == nil && x == controlCheckX && y == controlCheckY {
			desktop = DesktopControl
		}
	}
	reset, err := vncPassword()
	if err != nil {
		return "", err
	}
	defer clear(reset)
	if err := setDesktopPassword(ctx, g, reset, "the reset"); err != nil {
		return "", err
	}
	return desktop, nil
}

func setDesktopPassword(ctx context.Context, g ToolsGuest, pw []byte, which string) error {
	set, err := g.DesktopPassword(ctx, pw)
	switch {
	case err != nil:
		return fmt.Errorf("%w (%s password: %v)", ErrDesktopPassword, which, err)
	case !set:
		return fmt.Errorf("%w (%s password was not set)", ErrDesktopPassword, which)
	}
	return nil
}

// codeAlphabet has no look-alikes (no 0/O, 1/l/I).
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"

// randomCode is n characters from codeAlphabet, from crypto/rand without
// modulo bias.
func randomCode(n int) ([]byte, error) {
	out := make([]byte, 0, n)
	b := make([]byte, 1)
	limit := byte(256 - 256%len(codeAlphabet))
	for len(out) < n {
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		if b[0] < limit {
			out = append(out, codeAlphabet[int(b[0])%len(codeAlphabet)])
		}
	}
	return out, nil
}

// vncPassword is an 8-character VNC password: VncAuth uses 8 significant
// bytes.
func vncPassword() ([]byte, error) { return randomCode(8) }
