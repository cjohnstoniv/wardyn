// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Old org and old runner structs: the fields before local attach. The wire decoder is plain
// json.Unmarshal, so an old peer ignores what it does not know.
type (
	oldCaps struct {
		Version      string `json:"version"`
		BYOISelftest bool   `json:"byoi_selftest,omitempty"`
	}
	oldAuthOK struct {
		SessionID  string `json:"session_id"`
		ProxyImage string `json:"proxy_image,omitempty"`
	}
)

func TestLocalAttachFieldsAreOptionalBothWays(t *testing.T) {
	newCaps, _ := json.Marshal(Caps{Version: "0.10", BYOISelftest: true, LocalAttach: true})
	var oc oldCaps
	if err := Unmarshal(newCaps, &oc); err != nil || oc.Version != "0.10" || !oc.BYOISelftest {
		t.Fatalf("an old org decoding a new runner's caps = %+v, %v", oc, err)
	}
	newOK, _ := json.Marshal(AuthOK{SessionID: "s", ProxyImage: "p", LocalAttach: true})
	var ook oldAuthOK
	if err := Unmarshal(newOK, &ook); err != nil || ook.SessionID != "s" || ook.ProxyImage != "p" {
		t.Fatalf("an old runner decoding a new AUTH_OK = %+v, %v", ook, err)
	}
	var c Caps
	var a AuthOK
	if err := Unmarshal([]byte(`{"version":"0.9.5"}`), &c); err != nil || c.LocalAttach {
		t.Fatalf("an old runner's caps read as local attach: %+v, %v", c, err)
	}
	if err := Unmarshal([]byte(`{"session_id":"s"}`), &a); err != nil || a.LocalAttach {
		t.Fatalf("an old org's AUTH_OK read as local attach: %+v, %v", a, err)
	}
	off, _ := json.Marshal(Caps{Version: "x"})
	on, _ := json.Marshal(AuthOK{})
	if containsKey(off, "local_attach") || containsKey(on, "local_attach") {
		t.Fatalf("an unset local_attach is sent: %s %s", off, on)
	}
}

func containsKey(b []byte, key string) bool {
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	_, ok := m[key]
	return ok
}

func TestLocalAttachMessagesRoundTrip(t *testing.T) {
	check := func(in, out any) {
		t.Helper()
		b, err := Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
		if got, _ := Marshal(out); string(got) != string(b) {
			t.Fatalf("round trip %s -> %s", b, got)
		}
	}
	check(LocalAttachArgs{Ref: "runner:r/c", Nonce: "n", Tee: true}, &LocalAttachArgs{})
	check(LocalAttachModeArgs{Stream: 7, Observer: true, Cols: 80, Rows: 24}, &LocalAttachModeArgs{})
	check(LocalAttachTeeAckArgs{Stream: 7, Offset: 1 << 40}, &LocalAttachTeeAckArgs{})
	// An older runner answers a call it does not know with a refusal, which Call surfaces and the
	// session survives: the new methods are plain calls, not frame types.
	for _, m := range []string{MethodLocalAttach, MethodLocalAttachMode, MethodLocalAttachTeeAck} {
		p := newPair(t, PeerConfig{}, PeerConfig{OnCall: func(_ context.Context, _ uint32, method string, _ json.RawMessage) (any, error) {
			return nil, Refuse("unknown runner method")
		}})
		var we *Error
		if err := p.org.Call(ctxT(t), m, struct{}{}, nil); !errors.As(err, &we) || we.Code != CodeRefused || !p.org.Online() {
			t.Fatalf("%s to an older runner = %v, online %v", m, err, p.org.Online())
		}
	}
}

func TestLocalAttachTeeWindowIsPinned(t *testing.T) {
	if LocalAttachTeeWindowBytes != 64<<10 || LocalAttachTeeWindowTimeout != 2*time.Second {
		t.Fatalf("tee window = %d bytes, %v", LocalAttachTeeWindowBytes, LocalAttachTeeWindowTimeout)
	}
}

// openTee runs a local_attach call whose runner end opens a pty_tee stream, and returns both ends.
func openTee(t *testing.T, kind string) (p *pair, org, run *Stream, err error) {
	t.Helper()
	runSide := make(chan *Stream, 1)
	var runPeer *Peer
	h := func(ctx context.Context, id uint32, method string, _ json.RawMessage) (any, error) {
		s, oerr := runPeer.Open(ctx, Open{Kind: kind, Call: id})
		if oerr != nil {
			return nil, oerr
		}
		runSide <- s
		return StreamResult{Stream: s.ID()}, nil
	}
	p = newPair(t, PeerConfig{}, PeerConfig{OnCall: h})
	runPeer = p.run
	var res StreamResult
	if err = p.org.Call(ctxT(t), MethodLocalAttach, LocalAttachArgs{Ref: "r", Tee: true}, &res); err != nil {
		return p, nil, nil, err
	}
	org, err = p.org.TakeStream(res.Stream)
	return p, org, <-runSide, err
}

func TestLocalAttachTeeFlowsRunnerToOrgOnly(t *testing.T) {
	_, org, run, err := openTee(t, KindPTYTee)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Write([]byte("out")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if n, err := org.Read(buf); err != nil || string(buf[:n]) != "out" {
		t.Fatalf("org read = %q, %v", buf[:n], err)
	}
	// The org's own Write is refused and resets the stream, so the runner ends the session.
	if _, err := org.Write([]byte("in")); err == nil {
		t.Fatal("the org wrote to a pty_tee stream")
	}
	var re *ResetError
	if _, err := run.Read(buf); !errors.As(err, &re) || re.Code != ResetRefused {
		t.Fatalf("runner read after an org write = %v, want reset refused", err)
	}
}

// A DATA frame the org sends on the tee anyway (bypassing Stream.Write) resets it at the runner.
func TestLocalAttachTeeRunnerResetsOrgData(t *testing.T) {
	p, org, run, err := openTee(t, KindPTYTee)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.org.send(ctxT(t), Frame{Type: TypeData, Stream: org.ID(), Payload: []byte("in")}); err != nil {
		t.Fatal(err)
	}
	_ = run.SetReadDeadline(time.Now().Add(2 * time.Second))
	var re *ResetError
	if _, err := run.Read(make([]byte, 1)); !errors.As(err, &re) || re.Code != ResetRefused {
		t.Fatalf("runner read after org DATA = %v, want reset refused", err)
	}
}

// pty_tee is accepted only as the reply stream of a local_attach call.
func TestLocalAttachTeeOpenIsRefusedOutsideALocalAttachCall(t *testing.T) {
	expectRefused := func(t *testing.T, r *rawPeer, call uint32) {
		t.Helper()
		open, _ := json.Marshal(Open{Kind: KindPTYTee, Call: call})
		r.write(t, Frame{Type: TypeOpen, Stream: 1, Payload: open})
		rst := r.readType(t, TypeReset)
		if code, _ := DecodeReset(rst.Payload); rst.Stream != 1 || code != ResetRefused {
			t.Fatalf("RESET = stream %d code %d, want stream 1 refused", rst.Stream, code)
		}
	}
	t.Run("no call", func(t *testing.T) { expectRefused(t, newRaw(t, PeerConfig{Org: true}), 0) })
	t.Run("another call", func(t *testing.T) {
		r := newRaw(t, PeerConfig{Org: true})
		go func() { _ = r.p.Call(ctxT(t), MethodAttach, nil, nil) }()
		call := r.readType(t, TypeCall)
		expectRefused(t, r, call.Stream)
	})
	t.Run("runner side", func(t *testing.T) {
		// Only the org accepts this kind: it is the runner's reply to the org's local_attach call.
		r := newRaw(t, PeerConfig{Org: false})
		open, _ := json.Marshal(Open{Kind: KindPTYTee, Call: 0})
		r.write(t, Frame{Type: TypeOpen, Stream: 2, Payload: open})
		rst := r.readType(t, TypeReset)
		if code, _ := DecodeReset(rst.Payload); code != ResetRefused {
			t.Fatalf("RESET code %d, want refused", code)
		}
	})
	t.Run("local_attach call", func(t *testing.T) {
		r := newRaw(t, PeerConfig{Org: true})
		go func() { _ = r.p.Call(ctxT(t), MethodLocalAttach, nil, nil) }()
		call := r.readType(t, TypeCall)
		open, _ := json.Marshal(Open{Kind: KindPTYTee, Call: call.Stream})
		r.write(t, Frame{Type: TypeOpen, Stream: 1, Payload: open})
		r.readType(t, TypeOpenOK)
	})
}
