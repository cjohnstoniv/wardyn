// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func BenchmarkWebSocketDataFrames(b *testing.B) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ws := NewWebSocket(conn)
		for {
			f, err := ws.ReadFrame(ctx)
			if err != nil {
				return
			}
			if err := ws.WriteFrame(ctx, f); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.CloseNow()
	ws := NewWebSocket(conn)
	f := Frame{Type: TypeData, Stream: 1, Payload: bytes.Repeat([]byte{9}, MaxDataFrame)}
	b.SetBytes(MaxDataFrame)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ws.WriteFrame(ctx, f); err != nil {
			b.Fatal(err)
		}
		if _, err := ws.ReadFrame(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
