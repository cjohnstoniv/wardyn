// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package localviewer

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/jezek/xgb/xproto"
)

// The keysyms the tables use.
const (
	ksKPHome = 0xff95
	ksKP7    = 0xffb7
	numLock  = 1 << 4 // Mod2, where testKeyboard's Num_Lock key sits
)

// testKeyboard is a keyboard mapping from keycode 8, two columns each: a, 1, the keypad's 7, Num_Lock, b
// (no shifted column), x (no plain column) and c with Num_Lock shifted.
func testKeyboard(numLockBit uint16) *input {
	return &input{min: 8, per: 2, numLock: numLockBit, down: map[xproto.Keycode]uint32{}, keysyms: []xproto.Keysym{
		'a', 'A', // 8
		'1', '!', // 9
		ksKPHome, ksKP7, // 10
		keysymNumLock, 0, // 11
		'b', 0, // 12
		0, 'x', // 13: nothing in the plain column
		'c', keysymNumLock, // 14: Num_Lock in the shifted column
	}}
}

func TestKeysym(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nl    uint16
		kc    xproto.Keycode
		state uint16
		want  uint32
	}{
		{"letter", 0, 8, 0, 'a'},
		{"letter with Shift", 0, 8, shiftMask, 'A'},
		{"letter with Caps Lock", 0, 8, lockMask, 'A'},
		{"letter with Caps Lock and Shift", 0, 8, lockMask | shiftMask, 'a'},
		{"digit", 0, 9, 0, '1'},
		{"digit with Shift", 0, 9, shiftMask, '!'},
		{"digit with Caps Lock", 0, 9, lockMask, '1'},
		{"digit with Caps Lock and Shift", 0, 9, lockMask | shiftMask, '!'},
		{"no shifted column", 0, 12, shiftMask, 'b'},
		{"keycode below the table", 0, 7, 0, 0},
		{"keycode past the table", 0, 15, 0, 0},
		{"no plain column", 0, 13, 0, 0},
		{"no plain column with Shift", 0, 13, shiftMask, 'x'},
		{"keypad, Num Lock off", numLock, 10, 0, ksKPHome},
		{"keypad, Num Lock on", numLock, 10, numLock, ksKP7},
		{"keypad, Num Lock on with Shift", numLock, 10, numLock | shiftMask, ksKPHome},
		{"keypad, Num Lock on with Caps Lock", numLock, 10, numLock | lockMask, ksKP7},
		{"keypad, Num Lock off with Shift", numLock, 10, shiftMask, ksKP7},
		{"keypad, no Num Lock modifier known", 0, 10, numLock, ksKPHome},
		{"letter, Num Lock on", numLock, 8, numLock, 'a'},
	} {
		if got := testKeyboard(tc.nl).keysym(tc.kc, tc.state); got != tc.want {
			t.Errorf("%s: keysym = %#x, want %#x", tc.name, got, tc.want)
		}
	}
}

func TestNumLockMask(t *testing.T) {
	in := testKeyboard(0)
	mods := make([]xproto.Keycode, 16) // 8 modifiers, 2 keycodes each
	mods[0] = 50                       // Shift: a keycode outside the table
	mods[9] = 11                       // Mod2's second keycode types Num_Lock
	if got := in.numLockMask(mods, 2); got != 1<<4 {
		t.Errorf("Num_Lock under Mod2: mask %#x, want %#x", got, 1<<4)
	}
	if got := in.numLockMask(mods[:8], 2); got != 0 {
		t.Errorf("a short modifier mapping: mask %#x, want 0", got)
	}
	if got := in.numLockMask(make([]xproto.Keycode, 16), 2); got != 0 {
		t.Errorf("no Num_Lock key: mask %#x, want 0", got)
	}
	// X names the Num Lock modifier among Mod1 to Mod5 only.
	for slot, name := range []string{"Shift", "Lock", "Control"} {
		below := make([]xproto.Keycode, 16)
		below[slot*2] = 11
		if got := in.numLockMask(below, 2); got != 0 {
			t.Errorf("Num_Lock under %s: mask %#x, want 0", name, got)
		}
	}
	col1 := make([]xproto.Keycode, 16)
	col1[5*2] = 14 // Mod3's first keycode has Num_Lock in its shifted column
	if got := in.numLockMask(col1, 2); got != 1<<5 {
		t.Errorf("Num_Lock in column 1 under Mod3: mask %#x, want %#x", got, 1<<5)
	}
}

// keyEvents decodes the KeyEvents written to b as down/up and keysym.
func keyEvents(t *testing.T, b []byte) [][2]uint32 {
	t.Helper()
	var out [][2]uint32
	for ; len(b) >= 8; b = b[8:] {
		if b[0] != msgKeyEvent {
			t.Fatalf("message type %d, want a KeyEvent", b[0])
		}
		out = append(out, [2]uint32{uint32(b[1]), binary.BigEndian.Uint32(b[4:])})
	}
	if len(b) != 0 {
		t.Fatalf("%d bytes left over", len(b))
	}
	return out
}

func testSession() (*session, *bytes.Buffer) {
	var buf bytes.Buffer
	return &session{w: &buf, fb: &Framebuffer{W: 100, H: 50}}, &buf
}

func TestKeyReleaseSendsThePressKeysym(t *testing.T) {
	in := testKeyboard(0)
	s, buf := testSession()
	steps := []struct {
		down  bool
		kc    xproto.Keycode
		state uint16
	}{
		{true, 8, shiftMask}, // A goes down
		{false, 8, 0},        // Shift already up: the release is still A
		{false, 9, 0},        // a release with no press sends nothing
		{true, 7, 0},         // a keycode with no keysym sends nothing
		{false, 7, 0},
	}
	for _, st := range steps {
		if err := in.key(s, st.down, st.kc, st.state); err != nil {
			t.Fatal(err)
		}
	}
	got := keyEvents(t, buf.Bytes())
	want := [][2]uint32{{1, 'A'}, {0, 'A'}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if len(in.down) != 0 {
		t.Fatalf("keys still held: %v", in.down)
	}
}

func TestReleaseAll(t *testing.T) {
	in := testKeyboard(0)
	s, buf := testSession()
	for _, kc := range []xproto.Keycode{8, 9} {
		if err := in.key(s, true, kc, 0); err != nil {
			t.Fatal(err)
		}
	}
	buf.Reset()
	if err := in.releaseAll(s); err != nil {
		t.Fatal(err)
	}
	got := map[[2]uint32]bool{}
	for _, e := range keyEvents(t, buf.Bytes()) {
		got[e] = true
	}
	if len(got) != 2 || !got[[2]uint32{0, 'a'}] || !got[[2]uint32{0, '1'}] {
		t.Fatalf("releases = %v, want a and 1 up", got)
	}
	if len(in.down) != 0 {
		t.Fatalf("keys still held: %v", in.down)
	}
	buf.Reset()
	if err := in.releaseAll(s); err != nil || buf.Len() != 0 {
		t.Fatalf("a second releaseAll wrote %d bytes (err %v)", buf.Len(), err)
	}
}

func TestButtonMask(t *testing.T) {
	in := testKeyboard(0)
	s, buf := testSession()
	for _, tc := range []struct {
		down  bool
		b     byte
		sends bool
		mask  byte
	}{
		{true, 0, false, 0},
		{true, 1, true, 0x01},
		{true, 3, true, 0x05},
		{true, 4, true, 0x0d},
		{false, 4, true, 0x05},
		{true, 7, true, 0x45},
		{true, 8, false, 0x45},
		{false, 1, true, 0x44},
	} {
		buf.Reset()
		if err := in.button(s, tc.down, tc.b, 120, 7); err != nil {
			t.Fatal(err)
		}
		b := buf.Bytes()
		if !tc.sends {
			if len(b) != 0 || in.mask != tc.mask {
				t.Fatalf("button %d: wrote %d bytes, mask %#x; want nothing and %#x", tc.b, len(b), in.mask, tc.mask)
			}
			continue
		}
		want := []byte{msgPointerEvent, tc.mask, 0, 99, 0, 7} // x clamped to the 100-wide desktop
		if !bytes.Equal(b, want) {
			t.Fatalf("button %d down=%v: sent % x, want % x", tc.b, tc.down, b, want)
		}
	}
}

func TestChannel(t *testing.T) {
	for _, tc := range []struct {
		mask  uint32
		shift uint8
		max   uint16
	}{
		{0, 0, 0},
		{0xff, 0, 255},
		{0xff00, 8, 255},
		{0xff0000, 16, 255},
		{0x3ff00000, 20, 1023},
	} {
		if shift, maxV := channel(tc.mask); shift != tc.shift || maxV != tc.max {
			t.Errorf("channel(%#x) = %d, %d; want %d, %d", tc.mask, shift, maxV, tc.shift, tc.max)
		}
	}
}
