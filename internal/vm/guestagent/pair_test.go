// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// testPairKey is printable, so a leak of it shows in any text it reaches.
var testPairKey = []byte("wardyn-test-pair-key-0123456789!")

// The proof is HMAC-SHA256(key, "wardyn-guest-pair-v1" || port (big-endian, 32 bits) || challenge). The
// port is inside it, so an answer for one channel is no answer on another.
func TestPairProofIsHMACOverPortAndChallenge(t *testing.T) {
	challenge := bytes.Repeat([]byte{0xa5}, 32)
	mac := hmac.New(sha256.New, testPairKey)
	mac.Write([]byte("wardyn-guest-pair-v1"))
	mac.Write(binary.BigEndian.AppendUint32(nil, VsockDesktopPort))
	mac.Write(challenge)
	if got := PairProof(testPairKey, VsockDesktopPort, challenge); !hmac.Equal(got, mac.Sum(nil)) {
		t.Fatalf("proof %x, want %x", got, mac.Sum(nil))
	}
	if hmac.Equal(PairProof(testPairKey, VsockControlPort, challenge), PairProof(testPairKey, VsockDesktopPort, challenge)) {
		t.Fatal("a proof for 1025 is also one for 1024")
	}
	other := bytes.Repeat([]byte{0x5a}, 32)
	if hmac.Equal(PairProof(testPairKey, VsockToolsPort, challenge), PairProof(testPairKey, VsockToolsPort, other)) {
		t.Fatal("the proof does not depend on the challenge")
	}
	if hmac.Equal(PairProof(testPairKey, VsockToolsPort, challenge), PairProof(bytes.Repeat([]byte{1}, 32), VsockToolsPort, challenge)) {
		t.Fatal("the proof does not depend on the key")
	}
	if MacPairKeyPath != "/Library/Wardyn/pair.key" {
		t.Fatalf("the guest reads its key from %s, not the file stage 1 writes", MacPairKeyPath)
	}
}

// An older helper sends no challenge: what reaches the guest first is runnerd's session hello, the desktop
// open byte, or nothing. The guest closes the connection, writes nothing, and never serves on it.
func TestGuestRefusesAHostThatDoesNotChallenge(t *testing.T) {
	hello := make([]byte, helloLen)
	hello[0] = helloVersion
	const refused = "guestagent: the host did not authenticate the pairing (an older runner)"
	for name, c := range map[string]struct {
		host  func(h net.Conn)
		wants string // the error's text, "" for any error
	}{
		"a session hello":       {host: func(h net.Conn) { _, _ = h.Write(appendRecord([]byte{0}, recHello, hello)) }, wants: refused},
		"the desktop open byte": {host: func(h net.Conn) { _, _ = h.Write([]byte{DesktopOpenByte}) }, wants: refused},
		"a closed connection":   {host: func(h net.Conn) { _ = h.Close() }},
	} {
		t.Run(name, func(t *testing.T) {
			host, guest := net.Pipe()
			defer host.Close()
			wrote := make(chan []byte, 1)
			go func() {
				c.host(host)
				b, _ := io.ReadAll(host) // what the guest writes back before it closes
				wrote <- b
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := PairedDial(func(context.Context) (net.Conn, error) { return guest, nil }, VsockControlPort, testPairKey)(ctx)
			if err == nil || conn != nil {
				t.Fatalf("paired with a host that sent no challenge: %v %v", conn, err)
			}
			if c.wants != "" && !strings.Contains(err.Error(), c.wants) {
				t.Fatalf("error %q, want %q", err, c.wants)
			}
			select {
			case b := <-wrote:
				if len(b) != 0 {
					t.Fatalf("the guest wrote %x to a host that sent no challenge", b)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the guest did not close the connection")
			}
		})
	}
	t.Run("a silent host", func(t *testing.T) {
		old := pairChallengeWait
		pairChallengeWait = 100 * time.Millisecond
		defer func() { pairChallengeWait = old }()
		host, guest := net.Pipe()
		defer host.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := PairedDial(func(context.Context) (net.Conn, error) { return guest, nil }, VsockControlPort, testPairKey)(ctx)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), refused) {
				t.Fatalf("error %v, want %q", err, refused)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the guest waited past its challenge deadline")
		}
		_ = host.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := host.Write([]byte{0}); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("the guest kept the connection open: %v", err)
		}
	})
}

// The host's challenge is "WDYNPAIR" and 32 bytes. The guest answers with the 32-byte proof alone, and
// the connection it hands back carries the host's next bytes.
func TestPairedDialAnswersTheChallengeFrame(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	challenge := bytes.Repeat([]byte{0x3c}, 32)
	answer := make(chan []byte, 1)
	go func() {
		_ = host.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := host.Write(append([]byte("WDYNPAIR"), challenge...)); err != nil {
			answer <- nil
			return
		}
		b := make([]byte, 32)
		_ = host.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(host, b); err != nil {
			answer <- nil
			return
		}
		answer <- b
		_, _ = host.Write([]byte("next"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := PairedDial(func(context.Context) (net.Conn, error) { return guest, nil }, VsockToolsPort, testPairKey)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	mac := hmac.New(sha256.New, testPairKey)
	mac.Write([]byte("wardyn-guest-pair-v1"))
	mac.Write(binary.BigEndian.AppendUint32(nil, VsockToolsPort))
	mac.Write(challenge)
	var got []byte
	select {
	case got = <-answer:
	case <-time.After(5 * time.Second):
	}
	if !hmac.Equal(got, mac.Sum(nil)) {
		t.Fatalf("answer %x, want the proof %x", got, mac.Sum(nil))
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 4)
	if _, err := io.ReadFull(conn, b); err != nil || string(b) != "next" {
		t.Fatalf("after the proof the connection read %q %v", b, err)
	}
}
