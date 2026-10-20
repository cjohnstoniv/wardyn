// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// endlessCast streams zeros and gives its slot back on Close, as a PGStore cast does.
type endlessCast struct{ release func() }

func (endlessCast) Read(p []byte) (int, error) { return len(p), nil }
func (c endlessCast) Close() error             { c.release(); return nil }

type slotStore struct {
	recording.Store
	slots *recording.SlotQueue
}

func (s slotStore) OpenCast(ctx context.Context, _ string) (io.ReadCloser, error) {
	release, err := s.slots.Take(ctx)
	if err != nil {
		return nil, err
	}
	return endlessCast{release: release}, nil
}

// A reader that stops reading must not hold a read slot past the replay write bound.
func TestHandler_StalledReaderFreesItsSlot(t *testing.T) {
	defer recording.SetReplayWriteTimeout(300 * time.Millisecond)()
	const runID = "0a000000-0000-4000-8000-000000000001"
	key := recording.CastKey(runID, "s1")
	store := slotStore{slots: recording.NewSlotQueue(1)}
	srv := httptest.NewServer(newTestRouter(store))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(1024)
	}
	if _, err := fmt.Fprintf(conn, "GET /api/v1/runs/%s/recording/%s HTTP/1.1\r\nHost: x\r\n\r\n", runID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(conn).Peek(1); err != nil {
		t.Fatalf("no response: %v", err)
	}
	// The client reads nothing more: the socket buffers fill and the server's writes block.
	deadline := time.Now().Add(5 * time.Second)
	for store.slots.Held() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a stalled reader still holds the read slot after the write bound")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
