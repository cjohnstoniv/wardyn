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
	numLock uint16 // the modifier bit Num_Lock sets in a key event's state; 0 when no modifier holds it
}

func newInput(x *xgb.Conn) *input {
	setup := xproto.Setup(x)
	in := &input{min: setup.MinKeycode, down: map[xproto.Keycode]uint32{}}
	r, err := xproto.GetKeyboardMapping(x, setup.MinKeycode, byte(setup.MaxKeycode-setup.MinKeycode+1)).Reply()
	if err == nil {
		in.per, in.keysyms = int(r.KeysymsPerKeycode), r.Keysyms
	}
	if m, err := xproto.GetModifierMapping(x).Reply(); err == nil {
		in.numLock = in.numLockMask(m.Keycodes, int(m.KeycodesPerModifier))
	}
	return in
}

// The keysyms the keypad rule reads.
const (
	keysymNumLock = 0xff7f
	keysymKPFirst = 0xff80 // KP_Space
	keysymKPLast  = 0xffbd // KP_9
	modifierSlots = 8      // Shift, Lock, Control, Mod1 to Mod5
)

// numLockMask is the state bit of the modifier whose keycodes (the modifier mapping's, perMod a slot)
// include one that types Num_Lock.
func (in *input) numLockMask(mods []xproto.Keycode, perMod int) uint16 {
	for slot := range modifierSlots {
		for j := range perMod {
			k := slot*perMod + j
			if k >= len(mods) || mods[k] == 0 {
				continue
			}
			if in.column(mods[k], 0) == keysymNumLock || in.column(mods[k], 1) == keysymNumLock {
				return 1 << slot
			}
		}
	}
	return 0
}

// column is keycode kc's keysym in column c of the keyboard mapping, 0 outside the table.
func (in *input) column(kc xproto.Keycode, c int) uint32 {
	i := (int(kc) - int(in.min)) * in.per
	if in.per <= c || i < 0 || i+in.per > len(in.keysyms) {
		return 0
	}
	return uint32(in.keysyms[i+c])
}

func isKeypad(ks uint32) bool { return ks >= keysymKPFirst && ks <= keysymKPLast }

// keysym is the keysym kc types with the modifiers in state: the shifted column under Shift, Caps Lock
// shifting letters only. With Num Lock on, a key whose shifted column is a keypad keysym types it (KP_7
// rather than KP_Home), and Shift then gives the plain column, as X itself decides.
func (in *input) keysym(kc xproto.Keycode, state uint16) uint32 {
	plain, shifted := in.column(kc, 0), in.column(kc, 1)
	if plain == 0 {
		return 0
	}
	if shifted == 0 {
		shifted = plain
	}
	shift := state&shiftMask != 0
	if in.numLock != 0 && state&in.numLock != 0 && isKeypad(shifted) {
		if shift {
			return plain
		}
		return shifted
	}
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
