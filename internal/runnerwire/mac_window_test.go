// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// window is absent from an older runner's caps and unset ones are not sent, so an older org reads
// nothing new.
func TestMacWindowCapIsOptionalBothWays(t *testing.T) {
	var c MacVMCaps
	if err := Unmarshal([]byte(`{"max":1}`), &c); err != nil || c.Window {
		t.Fatalf("an older runner's caps read as a window: %+v, %v", c, err)
	}
	off, _ := json.Marshal(MacVMCaps{Max: 1})
	on, _ := json.Marshal(MacVMCaps{Max: 1, Window: true})
	if containsKey(off, "window") || !containsKey(on, "window") {
		t.Fatalf("window on the wire: unset %s, set %s", off, on)
	}
	var old struct {
		Max int `json:"max"`
	}
	if err := Unmarshal(on, &old); err != nil || old.Max != 1 {
		t.Fatalf("an older org decoding window caps = %+v, %v", old, err)
	}
}

func TestMacWindowMessagesAreGolden(t *testing.T) {
	for in, golden := range map[any]string{
		MacWindowArgs{Ref: "runner:r/c", GrantID: "g1", Title: "Run 7, for Pat"}: `{"ref":"runner:r/c","grant_id":"g1","title":"Run 7, for Pat"}`,
		MacWindowArgs{Ref: "runner:r/c", GrantID: "g1"}:                          `{"ref":"runner:r/c","grant_id":"g1"}`,
		MacWindowEnd{Reason: "closed_by_person"}:                                 `{"reason":"closed_by_person"}`,
	} {
		b, err := Marshal(in)
		if err != nil || string(b) != golden {
			t.Fatalf("%T wire form %s (%v), want %s", in, b, err, golden)
		}
	}
	at := time.Now().UTC().Truncate(time.Second)
	b, err := Marshal(MacWindowUnexpected{Grant: "x1", At: at})
	var back MacWindowUnexpected
	if err != nil || !strings.HasPrefix(string(b), `{"grant":"x1","at":"`) || Unmarshal(b, &back) != nil || back.Grant != "x1" || !back.At.Equal(at) {
		t.Fatalf("unexpected-window wire form %s (%v) reads back as %+v, want grant x1 at %v", b, err, back, at)
	}
	if MethodMacWindow != "mac_window" || KindMacWindow != "mac_window" {
		t.Fatalf("names %q %q", MethodMacWindow, KindMacWindow)
	}
	if err := (Open{Kind: KindMacWindow}).Validate(); err != nil {
		t.Fatalf("the window stream kind is refused: %v", err)
	}
}

// The sentences are the same on every side that refuses; pin them.
func TestMacWindowSentencesArePinned(t *testing.T) {
	for got, want := range map[string]string{
		MacWindowOpenSentence:        "This run's window is already open.",
		MacWindowNotMacSentence:      "This run is not a macOS VM, so it has no window to open.",
		MacWindowHelperOldSentence:   "This run's macOS VM helper is too old to open a window. End the run and start it again on this runner.",
		MacWindowNotAtMacSentence:    "The window didn't open: you aren't signed in at this Mac's screen, or it is locked. Unlock it and open the window again.",
		MacWindowUnreadableSentence:  "The window didn't open: Wardyn couldn't tell whether you are at this Mac's screen. Unlock it and open the window again.",
		MacWindowLockUnknownSentence: "Wardyn can't tell whether this Mac's screen is locked, so the window stays closed.",
	} {
		if got != want {
			t.Errorf("sentence %q, want %q", got, want)
		}
	}
}

// The tripwire is an event kind this build's validation accepts. An organisation that predates it fails
// validation on the kind and ends the link (the event is replayed on resume), so the organisation must be
// upgraded before any runner that sends it.
func TestMacWindowUnexpectedIsAnEventKind(t *testing.T) {
	if EventMacWindowUnexpected != "mac_window_unexpected" {
		t.Fatalf("kind %q", EventMacWindowUnexpected)
	}
	if err := (Event{Kind: EventMacWindowUnexpected}).Validate(); err != nil {
		t.Fatalf("the kind is refused: %v", err)
	}
}

// An older runner answers mac_window as any unknown method (a refusal the session survives). A newer
// runner answers with a mac_window stream: it flows runner to org, its end line arrives before EOF, and
// the org ends it by closing.
func TestMacWindowStreamEndsEitherWay(t *testing.T) {
	p := newPair(t, PeerConfig{}, PeerConfig{OnCall: func(context.Context, uint32, string, json.RawMessage) (any, error) {
		return nil, Refuse("unknown runner method")
	}})
	var we *Error
	if err := p.org.Call(ctxT(t), MethodMacWindow, MacWindowArgs{}, nil); !errors.As(err, &we) || we.Code != CodeRefused || !p.org.Online() {
		t.Fatalf("mac_window to an older runner = %v, online %v", err, p.org.Online())
	}

	_, org, run, err := openWindowStream(t)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(MacWindowEnd{Reason: "host_locked"})
	if _, err := run.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := run.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(org)
	if err != nil {
		t.Fatal(err)
	}
	var end MacWindowEnd
	if err := json.Unmarshal(b, &end); err != nil || end.Reason != "host_locked" {
		t.Fatalf("the end line %q = %+v, %v", b, end, err)
	}

	_, org, run, err = openWindowStream(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := org.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.Done():
	case <-ctxT(t).Done():
		t.Fatal("the org's close did not reach the runner's end")
	}
}

// A mac_window stream is the runner's reply to the org's mac_window call, and nothing else.
func TestMacWindowOpenIsRefusedOutsideAMacWindowCall(t *testing.T) {
	refused := func(t *testing.T, r *rawPeer, call uint32) {
		t.Helper()
		open, _ := json.Marshal(Open{Kind: KindMacWindow, Call: call})
		r.write(t, Frame{Type: TypeOpen, Stream: 1, Payload: open})
		rst := r.readType(t, TypeReset)
		if code, _ := DecodeReset(rst.Payload); rst.Stream != 1 || code != ResetRefused {
			t.Fatalf("RESET = stream %d code %d, want stream 1 refused", rst.Stream, code)
		}
	}
	t.Run("no call", func(t *testing.T) { refused(t, newRaw(t, PeerConfig{Org: true}), 0) })
	t.Run("another call", func(t *testing.T) {
		r := newRaw(t, PeerConfig{Org: true})
		go func() { _ = r.p.Call(ctxT(t), MethodLocalAttach, nil, nil) }()
		call := r.readType(t, TypeCall)
		refused(t, r, call.Stream)
	})
	t.Run("runner side", func(t *testing.T) {
		r := newRaw(t, PeerConfig{Org: false, OnOpen: func(*Stream, Open) error { return nil }})
		go func() { _ = r.p.Call(ctxT(t), MethodMacWindow, nil, nil) }()
		call := r.readType(t, TypeCall)
		open, _ := json.Marshal(Open{Kind: KindMacWindow, Call: call.Stream})
		r.write(t, Frame{Type: TypeOpen, Stream: 2, Payload: open})
		rst := r.readType(t, TypeReset)
		if code, _ := DecodeReset(rst.Payload); code != ResetRefused {
			t.Fatalf("RESET code %d, want refused", code)
		}
	})
	t.Run("mac_window call", func(t *testing.T) {
		r := newRaw(t, PeerConfig{Org: true})
		go func() { _ = r.p.Call(ctxT(t), MethodMacWindow, nil, nil) }()
		call := r.readType(t, TypeCall)
		open, _ := json.Marshal(Open{Kind: KindMacWindow, Call: call.Stream})
		r.write(t, Frame{Type: TypeOpen, Stream: 1, Payload: open})
		if ok := r.readType(t, TypeOpenOK); ok.Stream != 1 {
			t.Fatalf("OPEN_OK for stream %d, want 1", ok.Stream)
		}
	})
}

// The org never writes a window's stream: its own Write is refused, and DATA it sends anyway resets the
// stream at the runner.
func TestMacWindowStreamTakesNothingFromTheOrg(t *testing.T) {
	_, org, run, err := openWindowStream(t)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := org.Write([]byte("in")); err == nil {
		t.Fatal("the org wrote to a mac_window stream")
	}
	var re *ResetError
	if _, err := run.Read(make([]byte, 1)); !errors.As(err, &re) || re.Code != ResetRefused {
		t.Fatalf("runner read after an org write = %v, want reset refused", err)
	}
}

// openWindowStream runs a mac_window call whose runner end opens a mac_window stream, and returns both ends.
func openWindowStream(t *testing.T) (p *pair, org, run *Stream, err error) {
	t.Helper()
	runSide := make(chan *Stream, 1)
	var runPeer *Peer
	h := func(ctx context.Context, id uint32, _ string, _ json.RawMessage) (any, error) {
		s, oerr := runPeer.Open(ctx, Open{Kind: KindMacWindow, Call: id})
		if oerr != nil {
			return nil, oerr
		}
		runSide <- s
		return StreamResult{Stream: s.ID()}, nil
	}
	p = newPair(t, PeerConfig{}, PeerConfig{OnCall: h})
	runPeer = p.run
	var res StreamResult
	if err = p.org.Call(ctxT(t), MethodMacWindow, MacWindowArgs{Ref: "r", GrantID: "g"}, &res); err != nil {
		return p, nil, nil, err
	}
	org, err = p.org.TakeStream(res.Stream)
	return p, org, <-runSide, err
}
