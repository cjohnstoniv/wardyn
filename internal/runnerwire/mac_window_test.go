// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
		MacWindowArgs{Ref: "runner:r/c", GrantID: "g1", Title: "Run 7, for Pat"}:           `{"ref":"runner:r/c","grant_id":"g1","title":"Run 7, for Pat"}`,
		MacWindowUnexpected{Grant: "x1", At: time.Date(2026, 11, 3, 1, 2, 3, 0, time.UTC)}: `{"grant":"x1","at":"2026-11-03T01:02:03Z"}`,
		MacWindowArgs{Ref: "runner:r/c", GrantID: "g1"}:                                    `{"ref":"runner:r/c","grant_id":"g1"}`,
		MacWindowEnd{Reason: "closed_by_person"}:                                           `{"reason":"closed_by_person"}`,
	} {
		b, err := Marshal(in)
		if err != nil || string(b) != golden {
			t.Fatalf("%T wire form %s (%v), want %s", in, b, err, golden)
		}
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
		MacWindowOpenSentence:      "This run's window is already open.",
		MacWindowNotMacSentence:    "This run is not a macOS VM, so it has no window to open.",
		MacWindowHelperOldSentence: "This run's macOS VM helper is too old to open a window. End the run and start it again on this runner.",
	} {
		if got != want {
			t.Errorf("sentence %q, want %q", got, want)
		}
	}
}

// The tripwire is an event kind the org's validation accepts, and an older org reads it as unknown.
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

	_, org, run, err := openTee(t, KindMacWindow)
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

	_, org, run, err = openTee(t, KindMacWindow)
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
