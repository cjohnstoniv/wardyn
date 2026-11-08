// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package rfbauth_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/rfbauth"
	"github.com/cjohnstoniv/wardyn/internal/rfbauth/rfbauthtest"
)

// nextPrime is the first probable prime at or above n, found the same way on every run.
func nextPrime(n *big.Int) *big.Int {
	p := new(big.Int).Set(n)
	for !p.ProbablyPrime(20) {
		p.Add(p, big.NewInt(1))
	}
	return p
}

func TestARDClientAgainstAFakeServer(t *testing.T) {
	short := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19)) // 32 bytes
	odd := nextPrime(new(big.Int).Lsh(big.NewInt(1), 260))                          // 33 bytes
	for _, c := range []struct {
		name     string
		prime    *big.Int
		password string
	}{
		{"RFC 2409 group 2", nil, "s3cret-Agent"},
		{"a short key", short, "s3cret-Agent"},
		{"an odd-length key", odd, "s3cret-Agent"},
		{"a 63-byte password", nil, strings.Repeat("p", 63)},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := &rfbauthtest.Server{User: "agent", Password: c.password, Prime: c.prime}
			client := &rfbauth.ARDClient{Dial: srv.Dial}
			conn, si, err := client.Open(context.Background(), "agent", []byte(c.password))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			_ = conn.Close()
			if !bytes.Equal(si, rfbauthtest.ServerInit()) {
				t.Fatalf("ServerInit = %x, want the server's %x", si, rfbauthtest.ServerInit())
			}
			block := srv.Block()
			n := len(c.password)
			if string(block[:6]) != "agent\x00" || string(block[64:64+n+1]) != c.password+"\x00" {
				t.Fatalf("the credentials block does not hold the user and the password, NUL-terminated, at 0 and 64")
			}
			if bytes.Count(block[6:64], []byte{0}) == 58 || (n < 55 && bytes.Count(block[64+n+1:], []byte{0}) == 63-n) {
				t.Fatal("the credentials block is zero after the NULs, not random fill")
			}
		})
	}

	t.Run("a 64-byte password or user is refused before any dial", func(t *testing.T) {
		srv := &rfbauthtest.Server{User: "agent", Password: strings.Repeat("p", 64)}
		client := &rfbauth.ARDClient{Dial: srv.Dial}
		for _, cred := range [][2]string{{"agent", strings.Repeat("p", 64)}, {strings.Repeat("u", 64), "pw"}} {
			if _, _, err := client.Open(context.Background(), cred[0], []byte(cred[1])); !errors.Is(err, rfbauth.ErrCredentialTooLong) {
				t.Fatalf("Open with a %d-byte user and a %d-byte password = %v, want ErrCredentialTooLong", len(cred[0]), len(cred[1]), err)
			}
		}
		if srv.Conns() != 0 {
			t.Fatalf("the server took %d connections, want 0", srv.Conns())
		}
		if _, _, err := client.Open(context.Background(), "agent", []byte("pw")); errors.Is(err, rfbauth.ErrCredentialTooLong) {
			t.Fatal("a too-long credential stuck: it is not Screen Sharing's answer")
		}
	})

	t.Run("the connection carries the session after sign-in, past the sign-in's deadline", func(t *testing.T) {
		srv := &rfbauthtest.Server{User: "agent", Password: "pw", Session: func(c net.Conn) {
			defer c.Close()
			time.Sleep(400 * time.Millisecond)
			_, _ = io.WriteString(c, "frame")
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		conn, _, err := (&rfbauth.ARDClient{Dial: srv.Dial}).Open(ctx, "agent", []byte("pw"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer conn.Close()
		got := make([]byte, 5)
		if _, err := io.ReadFull(conn, got); err != nil || string(got) != "frame" {
			t.Fatalf("read after sign-in = %q, %v; want the server's bytes", got, err)
		}
	})
}

// After Screen Sharing refuses the sign-in or offers no Apple authentication, the client keeps that
// first answer for the rest of the boot: no later session signs in again as the account.
func TestARDClientKeepsTheFirstDeclineForTheBoot(t *testing.T) {
	const canary = "canary-Pa55word"
	t.Run("refused", func(t *testing.T) {
		srv := &rfbauthtest.Server{User: "agent", Password: canary}
		client := &rfbauth.ARDClient{Dial: srv.Dial}
		_, _, err := client.Open(context.Background(), "agent", []byte("wrong-"+canary))
		var first *rfbauth.DeclinedError
		if !errors.As(err, &first) || first.Reason != rfbauth.ReasonRefused || !strings.Contains(first.Detail, "authentication failed") {
			t.Fatalf("Open with a wrong password = %v, want a refused decline with the server's reason", err)
		}
		for _, s := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", first)} {
			if strings.Contains(s, canary) {
				t.Fatalf("the decline carries the password: %s", s)
			}
		}
		_, _, err = client.Open(context.Background(), "agent", []byte(canary))
		var second *rfbauth.DeclinedError
		if !errors.As(err, &second) || second != first {
			t.Fatalf("second Open = %v, want the first decline again", err)
		}
		if srv.Conns() != 1 || srv.ARDSignIns() != 1 {
			t.Fatalf("connections %d, sign-ins %d; want 1 and 1", srv.Conns(), srv.ARDSignIns())
		}
	})
	t.Run("no Apple authentication offered", func(t *testing.T) {
		srv := &rfbauthtest.Server{Offered: []byte{2, 5}, VNCPassword: "vnc"}
		client := &rfbauth.ARDClient{Dial: srv.Dial}
		_, _, err := client.Open(context.Background(), "agent", []byte(canary))
		var first *rfbauth.DeclinedError
		if !errors.As(err, &first) || first.Reason != rfbauth.ReasonNoAppleLogin || !bytes.Equal(first.Offered, []byte{2, 5}) {
			t.Fatalf("Open = %v, want a no_apple_login decline naming types [2 5]", err)
		}
		if _, _, err := client.Open(context.Background(), "agent", []byte(canary)); !errors.Is(err, first) {
			t.Fatalf("second Open = %v, want the first decline again", err)
		}
		if srv.Conns() != 1 || srv.ARDSignIns() != 0 {
			t.Fatalf("connections %d, sign-ins %d; want 1 and 0", srv.Conns(), srv.ARDSignIns())
		}
	})
	t.Run("a dropped connection is not a decline", func(t *testing.T) {
		dials := 0
		client := &rfbauth.ARDClient{Dial: func(context.Context) (net.Conn, error) {
			dials++
			near, far := net.Pipe()
			_ = far.Close()
			return near, nil
		}}
		for range 2 {
			var d *rfbauth.DeclinedError
			if _, _, err := client.Open(context.Background(), "agent", []byte(canary)); err == nil || errors.As(err, &d) {
				t.Fatalf("Open on a dropped connection = %v, want a plain error", err)
			}
		}
		if dials != 2 {
			t.Fatalf("dials = %d, want 2", dials)
		}
	})
}

func TestARDClientBoundsAStalledServer(t *testing.T) {
	near, far := net.Pipe()
	defer far.Close()
	client := &rfbauth.ARDClient{Dial: func(context.Context) (net.Conn, error) { return near, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := client.Open(ctx, "agent", []byte("pw"))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Open against a silent server = %v, want a deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open against a silent server still waits after 5s; its deadline is 200ms")
	}
}

// A view that closes during the sign-in cancels it at once, so the next view does not wait out the
// 15 s bound behind it; the cut is the caller's, not Screen Sharing's answer, so it is not kept.
func TestARDClientStopsWhenTheCallerCancels(t *testing.T) {
	srv := &rfbauthtest.Server{User: "agent", Password: "pw"}
	chose := make(chan struct{})
	dials := 0
	client := &rfbauth.ARDClient{Dial: func(ctx context.Context) (net.Conn, error) {
		if dials++; dials > 1 {
			return srv.Dial(ctx)
		}
		near, far := net.Pipe()
		go func() { // answers up to the client's choice of type 30, then says nothing
			defer far.Close()
			_, _ = io.WriteString(far, "RFB 003.889\n")
			_, _ = io.ReadFull(far, make([]byte, 12))
			_, _ = far.Write([]byte{1, 30})
			_, _ = io.ReadFull(far, make([]byte, 1))
			close(chose)
			_, _ = io.Copy(io.Discard, far)
		}()
		return near, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-chose; cancel() }()
	done := make(chan error, 1)
	go func() {
		_, _, err := client.Open(ctx, "agent", []byte("pw"))
		done <- err
	}()
	select {
	case err := <-done:
		var d *rfbauth.DeclinedError
		if !errors.Is(err, context.Canceled) || errors.As(err, &d) {
			t.Fatalf("Open cancelled mid-exchange = %v, want the caller's cancellation and no decline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open still waits 5s after its caller cancelled")
	}
	conn, _, err := client.Open(context.Background(), "agent", []byte("pw"))
	if err != nil {
		t.Fatalf("Open after a cancelled one = %v, want a sign-in", err)
	}
	_ = conn.Close()
}
