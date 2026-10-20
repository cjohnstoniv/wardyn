// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"
)

// clientHello returns the first flight a TLS client sends for serverName.
func clientHello(t *testing.T, serverName string) []byte {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		_ = tls.Client(a, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}).Handshake()
	}()
	var out []byte
	buf := make([]byte, 4096)
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		n, err := b.Read(buf)
		out = append(out, buf[:n]...)
		if len(out) >= 5 && len(out) >= 5+int(binary.BigEndian.Uint16(out[3:5])) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = a.Close()
	_ = b.Close()
	return out
}

// splitRecords re-frames one handshake record as two TLS records.
func splitRecords(rec []byte, at int) []byte {
	payload := rec[5:]
	hdr := func(n int) []byte { return []byte{rec[0], rec[1], rec[2], byte(n >> 8), byte(n)} }
	out := append(hdr(at), payload[:at]...)
	out = append(out, hdr(len(payload)-at)...)
	return append(out, payload[at:]...)
}

// feed writes chunks into a pipe with a pause between them and returns the
// relay's view of the flight.
func feed(t *testing.T, pause time.Duration, chunks ...[]byte) (flight, error) {
	t.Helper()
	guest, relay := net.Pipe()
	defer guest.Close()
	defer relay.Close()
	go func() {
		for i, c := range chunks {
			if i > 0 {
				time.Sleep(pause)
			}
			if _, err := guest.Write(c); err != nil {
				return
			}
		}
	}()
	return readFlight(relay)
}

func TestReadFlight(t *testing.T) {
	defer func(h, f time.Duration) { helloBudget, firstWait = h, f }(helloBudget, firstWait)
	helloBudget, firstWait = 300*time.Millisecond, 300*time.Millisecond
	hello := clientHello(t, "Example.COM")

	t.Run("whole", func(t *testing.T) {
		fl, err := feed(t, 0, hello)
		if err != nil || fl.kind != flightTLS || fl.sni != "example.com" || !bytes.Equal(fl.bytes, hello) {
			t.Fatalf("kind %v sni %q %d/%d bytes err %v", fl.kind, fl.sni, len(fl.bytes), len(hello), err)
		}
	})
	t.Run("split across segments", func(t *testing.T) {
		fl, err := feed(t, 50*time.Millisecond, hello[:3], hello[3:40], hello[40:])
		if err != nil || fl.sni != "example.com" || !bytes.Equal(fl.bytes, hello) {
			t.Fatalf("sni %q err %v: a split ClientHello must be read whole", fl.sni, err)
		}
	})
	t.Run("split across records", func(t *testing.T) {
		split := splitRecords(hello, 20)
		fl, err := feed(t, 0, split)
		if err != nil || fl.sni != "example.com" || !bytes.Equal(fl.bytes, split) {
			t.Fatalf("sni %q err %v", fl.sni, err)
		}
	})
	t.Run("delayed past the budget", func(t *testing.T) {
		if _, err := feed(t, time.Second, hello[:10], hello[10:]); !errors.Is(err, errSNIMissing) {
			t.Fatalf("err %v, want errSNIMissing", err)
		}
	})
	t.Run("no SNI", func(t *testing.T) {
		if _, err := feed(t, 0, clientHello(t, "")); !errors.Is(err, errSNIMissing) {
			t.Fatalf("err %v, want errSNIMissing", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		huge := append([]byte{0x16, 3, 1, 0xff, 0xff}, make([]byte, maxClientHello)...)
		if _, err := feed(t, 0, huge); !errors.Is(err, errSNIMissing) {
			t.Fatalf("err %v, want errSNIMissing", err)
		}
	})
	t.Run("not TLS", func(t *testing.T) {
		fl, err := feed(t, 0, []byte("SSH-2.0-OpenSSH_9.9\r\n"))
		if err != nil || fl.kind != flightOther || string(fl.bytes) != "S" {
			t.Fatalf("kind %v bytes %q err %v", fl.kind, fl.bytes, err)
		}
	})
	t.Run("nothing sent", func(t *testing.T) {
		fl, err := feed(t, 0)
		if err != nil || fl.kind != flightNone {
			t.Fatalf("kind %v err %v", fl.kind, err)
		}
	})
}
