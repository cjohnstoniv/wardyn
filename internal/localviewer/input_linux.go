// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package localviewer

import (
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// The X modifier bits the keysym choice reads.
const (
	shiftMask = 1 << 0
	lockMask  = 1 << 1
)

// input maps X key and button events to RFB's. RFB keys are X11 keysyms, so a keycode maps through the X
// server's own keyboard mapping; RFB pointer buttons are X's, as bits 0 to 6 of one mask.
type input struct {
	min     xproto.Keycode
	per     int
	keysyms []xproto.Keysym
	down    map[xproto.Keycode]uint32 // keys pressed, with the keysym their press sent
	mask    byte
}

func newInput(x *xgb.Conn) *input {
	setup := xproto.Setup(x)
	in := &input{min: setup.MinKeycode, down: map[xproto.Keycode]uint32{}}
	r, err := xproto.GetKeyboardMapping(x, setup.MinKeycode, byte(setup.MaxKeycode-setup.MinKeycode+1)).Reply()
	if err == nil {
		in.per, in.keysyms = int(r.KeysymsPerKeycode), r.Keysyms
	}
	return in
}

// keysym is the keysym kc types with the modifiers in state: the shifted column under Shift, Caps Lock
// shifting letters only.
func (in *input) keysym(kc xproto.Keycode, state uint16) uint32 {
	i := (int(kc) - int(in.min)) * in.per
	if in.per == 0 || i < 0 || i+in.per > len(in.keysyms) {
		return 0
	}
	plain, shifted := uint32(in.keysyms[i]), uint32(0)
	if in.per > 1 {
		shifted = uint32(in.keysyms[i+1])
	}
	if shifted == 0 {
		shifted = plain
	}
	shift := state&shiftMask != 0
	if state&lockMask != 0 && isLetter(plain) {
		shift = !shift
	}
	if shift {
		return shifted
	}
	return plain
}

// isLetter is a Latin-1 lower-case letter keysym, the ones Caps Lock shifts.
func isLetter(ks uint32) bool { return ks >= 'a' && ks <= 'z' }

// key sends a key event. A release sends the keysym its press sent, whatever the modifiers are now.
func (in *input) key(s *session, down bool, kc xproto.Keycode, state uint16) error {
	ks, held := in.down[kc]
	switch {
	case down:
		ks = in.keysym(kc, state)
		if ks == 0 {
			return nil
		}
		in.down[kc] = ks
	case !held:
		return nil
	default:
		delete(in.down, kc)
	}
	return s.keyEvent(down, ks)
}

// releaseAll lets go of every key held when the window loses focus, so none stays down in the VM.
func (in *input) releaseAll(s *session) error {
	for kc, ks := range in.down {
		delete(in.down, kc)
		if err := s.keyEvent(false, ks); err != nil {
			return err
		}
	}
	return nil
}

// button sends a pointer event for X button b (1 to 7: left, middle, right, then the four scroll
// directions) going down or up at x,y.
func (in *input) button(s *session, down bool, b byte, x, y int) error {
	if b < 1 || b > 7 {
		return nil
	}
	bit := byte(1) << (b - 1)
	if down {
		in.mask |= bit
	} else {
		in.mask &^= bit
	}
	return s.pointerEvent(in.mask, x, y)
}
