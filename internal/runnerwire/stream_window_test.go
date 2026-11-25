// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The organisation grants a runner's offer up to StreamWindowLarge and nothing to an older runner or a
// short offer; the window values are pinned, since both sides of a link must agree on them.
func TestGrantStreamWindowKeepsOlderRunnersOnTheLegacyWindow(t *testing.T) {
	for offer, want := range map[uint32]uint32{
		0:                 0,
		StreamWindow - 1:  0,
		StreamWindow:      StreamWindow,
		StreamWindowLarge: StreamWindowLarge,
		64 << 20:          StreamWindowLarge,
	} {
		if got := GrantStreamWindow(offer); got != want {
			t.Errorf("GrantStreamWindow(%d) = %d, want %d", offer, got, want)
		}
	}
	if StreamWindowLarge != 4<<20 {
		t.Errorf("StreamWindowLarge = %d, want 4 MiB", StreamWindowLarge)
	}
	if got := connWindowFor(StreamWindowLarge); got != 16<<20 {
		t.Errorf("connWindowFor(StreamWindowLarge) = %d, want 16 MiB", got)
	}
	if got := connWindowFor(StreamWindow); got != ConnWindow {
		t.Errorf("connWindowFor(StreamWindow) = %d, want ConnWindow", got)
	}
}

// bufLen is how many received bytes s holds unread.
func bufLen(s *Stream) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buf)
}

// The agreed window is the sender's initial credit, per stream and per connection, and a writer to a
// reader that never reads stops after exactly one stream window.
func TestNegotiatedWindowSetsTheSendersInitialCredit(t *testing.T) {
	for _, tc := range []struct {
		name               string
		window             int64
		wantStream, wantCn int64
	}{
		{"negotiated", StreamWindowLarge, StreamWindowLarge, 4 * StreamWindowLarge},
		{"legacy", 0, StreamWindow, ConnWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accepted := make(chan *Stream, 1)
			p := newPair(t, PeerConfig{StreamWindow: tc.window},
				PeerConfig{StreamWindow: tc.window, OnOpen: func(s *Stream, o Open) error { accepted <- s; return nil }})
			s, err := p.org.Open(ctxT(t), Open{Kind: KindRelay})
			if err != nil {
				t.Fatal(err)
			}
			run := <-accepted
			if got := avail(s.send); got != tc.wantStream {
				t.Fatalf("stream credit = %d, want %d", got, tc.wantStream)
			}
			if got := avail(p.org.connSend); got != tc.wantCn {
				t.Fatalf("connection credit = %d, want %d", got, tc.wantCn)
			}
			total := tc.wantStream + 1
			wrote := make(chan error, 1)
			go func() {
				_, err := s.Write(make([]byte, total))
				if err == nil {
					err = s.CloseWrite()
				}
				wrote <- err
			}()
			waitFor(t, 3*time.Second, "the window to fill", func() bool { return int64(bufLen(run)) == tc.wantStream })
			select {
			case err := <-wrote:
				t.Fatalf("the writer finished past the window (%v)", err)
			case <-time.After(100 * time.Millisecond):
			}
			if got := int64(bufLen(run)); got != tc.wantStream {
				t.Fatalf("the reader holds %d unread bytes, want exactly %d", got, tc.wantStream)
			}
			if got := readAllWithin(t, run, "drain"); int64(len(got)) != total {
				t.Fatalf("read %d bytes, want %d", len(got), total)
			}
			if err := <-wrote; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// streamPastWindow sends window bytes of DATA on a stream nobody reads, checks they are all held, then
// one byte more, and returns the first frame the peer answers with.
func streamPastWindow(t *testing.T, r *rawPeer, id uint32, window int64) Frame {
	t.Helper()
	open, _ := json.Marshal(Open{Kind: KindRelay})
	r.write(t, Frame{Type: TypeOpen, Stream: id, Payload: open})
	chunk := bytes.Repeat([]byte{1}, MaxDataFrame)
	for range window / MaxDataFrame {
		r.write(t, Frame{Type: TypeData, Stream: id, Payload: chunk})
	}
	waitFor(t, 3*time.Second, "the window's bytes to be held", func() bool { return r.p.held.Load() == window })
	if !r.p.Online() {
		t.Fatalf("a stream within its window closed the session: %v", r.p.Err())
	}
	r.write(t, Frame{Type: TypeData, Stream: id, Payload: []byte{1}})
	return r.readType(t, TypeReset)
}

// A receiver holds exactly its agreed stream window unread; one byte more resets that stream only.
func TestReceiverEnforcesItsNegotiatedStreamWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    int64
		window int64
	}{{"negotiated", StreamWindowLarge, StreamWindowLarge}, {"legacy", 0, StreamWindow}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRaw(t, PeerConfig{Org: true, StreamWindow: tc.cfg, OnOpen: func(*Stream, Open) error { return nil }})
			rst := streamPastWindow(t, r, 1, tc.window)
			if code, _ := DecodeReset(rst.Payload); rst.Stream != 1 || code != ResetInternal {
				t.Fatalf("RESET = stream %d code %d", rst.Stream, code)
			}
			if !r.p.Online() {
				t.Fatal("one stream's overflow closed the connection")
			}
		})
	}
}

// With the negotiated window a link holds four full streams unread (connWindowFor), and the next byte on
// any stream closes the session: the connection bound is four streams, not every stream's window.
func TestReceiverEnforcesItsNegotiatedConnectionWindow(t *testing.T) {
	r := newRaw(t, PeerConfig{Org: true, StreamWindow: StreamWindowLarge, OnOpen: func(*Stream, Open) error { return nil }})
	open, _ := json.Marshal(Open{Kind: KindRelay})
	chunk := bytes.Repeat([]byte{1}, MaxDataFrame)
	for i := range 4 {
		id := uint32(2*i + 1)
		r.write(t, Frame{Type: TypeOpen, Stream: id, Payload: open})
		for range StreamWindowLarge / MaxDataFrame {
			r.write(t, Frame{Type: TypeData, Stream: id, Payload: chunk})
		}
	}
	waitFor(t, 3*time.Second, "four streams' windows to be held", func() bool { return r.p.held.Load() == 4*StreamWindowLarge })
	if !r.p.Online() {
		t.Fatalf("four full streams closed the session: %v", r.p.Err())
	}
	r.write(t, Frame{Type: TypeOpen, Stream: 9, Payload: open})
	_ = r.raw.WriteFrame(ctxT(t), Frame{Type: TypeData, Stream: 9, Payload: []byte{1}})
	r.closedWith(t, ErrBadFrame)
}

// The runner refuses an AUTH_OK window it did not offer, or one below StreamWindow.
func TestAuthOKStreamWindowOutsideTheOfferIsRefused(t *testing.T) {
	for _, tc := range []struct {
		answer, offer uint32
		ok            bool
	}{
		{0, 0, true},
		{0, StreamWindowLarge, true},
		{StreamWindow, StreamWindowLarge, true},
		{StreamWindowLarge, StreamWindowLarge, true},
		{StreamWindowLarge, 0, false},
		{StreamWindow - 1, 0, false},
		{StreamWindow - 1, StreamWindowLarge, false},
		{2 * StreamWindowLarge, StreamWindowLarge, false},
	} {
		payload, err := Marshal(AuthOK{SessionID: uuid.NewString(), PingInterval: PingInterval, StreamWindow: tc.answer})
		if err != nil {
			t.Fatal(err)
		}
		got, err := checkAuthOK(payload, nil, tc.offer)
		if tc.ok && (err != nil || got.StreamWindow != tc.answer) {
			t.Errorf("answer %d to offer %d refused: %v", tc.answer, tc.offer, err)
		}
		if !tc.ok && !errors.Is(err, ErrBadFrame) {
			t.Errorf("answer %d to offer %d = %v, want ErrBadFrame", tc.answer, tc.offer, err)
		}
	}
}

// The client offers StreamWindowLarge and builds its session with the window the organisation answers: the
// large one when granted, the legacy one when the answer carries none. A peer on that agreed config resets a
// stream one byte past it.
func TestClientAppliesTheAgreedWindow(t *testing.T) {
	for _, tc := range []struct {
		name               string
		answer             uint32
		wantStream, wantCn int64
	}{
		{"granted", StreamWindowLarge, StreamWindowLarge, 16 << 20},
		{"older organisation", 0, StreamWindow, ConnWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			pub, key, _ := ed25519.GenerateKey(rand.Reader)
			const orgURL = "https://org.example"
			row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256(orgURL), State: types.RunnerClaimed}
			org, run := Loopback()
			offered := make(chan uint32, 1)
			go func() {
				defer org.Close()
				auth, err := AuthenticateRunner(ctx, org, row, orgURL)
				if err != nil {
					return
				}
				offered <- auth.StreamWindow
				ok := AuthOK{SessionID: uuid.NewString(), PingInterval: PingInterval, StreamWindow: tc.answer}
				if WriteControl(ctx, org, TypeAuthOK, ok) != nil {
					return
				}
				// The fresh exchange: PENDING from the org, then the runner's STATE and READY.
				peer := NewPeer(org, PeerConfig{Org: true, StreamWindow: int64(tc.answer)})
				if peer.Start(ctx, 0) != nil {
					return
				}
				defer peer.Close()
				if payload, err := Marshal([]PendingAction{}); err == nil {
					_ = peer.Send(ctx, Frame{Type: TypePending, Payload: payload})
				}
				<-ctx.Done()
			}()
			windows := make(chan [2]int64, 1)
			hooks := idleHooks()
			hooks.StatePages = func(_ context.Context, send func(StatePage) error) error {
				return send(StatePage{InventoryID: uuid.New(), Final: true})
			}
			hooks.Online = func(p *Peer, _ bool) { windows <- [2]int64{p.streamWin, p.connWin} }
			client, err := NewClient(ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
				Dial: func(context.Context) (Conn, error) { return run, nil }})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- client.Connect(ctx) }()
			select {
			case got := <-offered:
				if got != StreamWindowLarge {
					t.Fatalf("the client offered %d, want StreamWindowLarge", got)
				}
			case <-ctx.Done():
				t.Fatal("the client sent no AUTH")
			}
			var got [2]int64
			select {
			case got = <-windows:
			case err := <-done:
				t.Fatalf("the session ended before it was online: %v", err)
			case <-ctx.Done():
				t.Fatal("the session never came online")
			}
			cancel()
			<-done
			if got != [2]int64{tc.wantStream, tc.wantCn} {
				t.Fatalf("the client's session uses windows %v, want [%d %d]", got, tc.wantStream, tc.wantCn)
			}
			r := newRaw(t, PeerConfig{StreamWindow: got[0], OnOpen: func(*Stream, Open) error { return nil }})
			if rst := streamPastWindow(t, r, 2, got[0]); rst.Stream != 2 {
				t.Fatalf("RESET for stream %d, want 2", rst.Stream)
			}
		})
	}
}

// An unset window is not sent, so an older peer reads the same AUTH and AUTH_OK it always did; a set one
// is named stream_window.
func TestOlderOrganisationGetsNoWindowFieldBack(t *testing.T) {
	for name, v := range map[string]any{"AUTH": Auth{}, "AUTH_OK": AuthOK{}} {
		b, err := Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if containsKey(b, "stream_window") {
			t.Errorf("%s with no window sends one: %s", name, b)
		}
	}
	b, _ := Marshal(Auth{StreamWindow: StreamWindowLarge})
	var a Auth
	if !containsKey(b, "stream_window") || Unmarshal(b, &a) != nil || a.StreamWindow != StreamWindowLarge {
		t.Fatalf("AUTH offer wire form %s", b)
	}
}
