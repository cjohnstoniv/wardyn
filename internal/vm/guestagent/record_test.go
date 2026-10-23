// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent/guestagenttest"
)

// guestagenttest keeps its own copy of the record encoding, as this package exports none of its
// own. Records go both ways between its Listener and this package's encoder and decoder through a
// socket, byte for byte, so a change to one copy that the other lacks fails here.
func TestGuestagenttestSpeaksTheseRecords(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	host, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	guest, err := guestagenttest.Listener(ln).Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	deadline := time.Now().Add(10 * time.Second)
	_ = host.SetDeadline(deadline)
	_ = guest.SetDeadline(deadline)

	// Each length around a COBS group's 254 bytes and the record limit, each as mixed bytes, all
	// 0x00 and all 0xFF. The last length is over a record's body, so it travels as two.
	var bodies [][]byte
	for _, n := range []int{0, 1, 253, 254, 255, maxBody, maxBody + 1} {
		mixed := make([]byte, n)
		for i := range mixed {
			mixed[i] = byte(i)
		}
		bodies = append(bodies, mixed, make([]byte, n), bytes.Repeat([]byte{0xff}, n))
	}

	// Host to guest: a hello, then the bodies in this package's records, read back through the
	// Listener whole and in order. An empty body is one empty data record.
	hello := slices.Concat([]byte{helloVersion}, bytes.Repeat([]byte{9}, nonceLen))
	out := appendRecord([]byte{0}, recHello, hello)
	for _, b := range bodies {
		if len(b) == 0 {
			out = appendRecord(out, recData, nil)
		}
		out = appendData(out, b)
	}
	go func() { _, _ = host.Write(out) }()
	want := slices.Concat(bodies...)
	got := make([]byte, len(want))
	if n, err := io.ReadFull(guest, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the Listener read %d of %d bytes (%v), or other bytes than this package wrote", n, len(want), err)
	}

	// Guest to host: the hello's echo, then each body written through the Listener, read back
	// through this package's decoder.
	r := bufio.NewReaderSize(host, maxRecord)
	next := func() (byte, []byte) {
		t.Helper()
		for {
			rec, err := r.ReadSlice(0)
			if err != nil {
				t.Fatalf("reading a record: %v", err)
			}
			if len(rec) == 1 {
				continue // the 0x00 ahead of the echo
			}
			typ, body, ok := decodeRecord(rec[:len(rec)-1])
			if !ok {
				t.Fatalf("the Listener wrote %x, which is not a record", rec)
			}
			return typ, body
		}
	}
	if typ, body := next(); typ != recHello || !bytes.Equal(body, hello) {
		t.Fatalf("the Listener echoed a %d record %x; want the hello %x", typ, body, hello)
	}
	go func() {
		for _, b := range bodies {
			if _, err := guest.Write(b); err != nil {
				return
			}
		}
	}()
	for i, b := range bodies {
		var got []byte
		for len(got) < len(b) {
			typ, body := next()
			if typ != recData || len(body) > maxBody {
				t.Fatalf("body %d: a %d record of %d bytes; want data records of at most %d", i, typ, len(body), maxBody)
			}
			got = append(got, body...)
		}
		if !bytes.Equal(got, b) {
			t.Fatalf("body %d (%d bytes) came back as %d other bytes", i, len(b), len(got))
		}
	}
}
