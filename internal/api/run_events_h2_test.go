// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func newH2EventsServer(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(panicFails(t, srv.Handler()))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

// An HTTP/2 peer that grants the stream no flow-control window blocks the
// handler's first flush, where neither the hold nor the revocation check runs.
// The batch's write deadline must still end the handler and free the
// principal's stream slot while the peer keeps the connection open.
func TestRunEvents_ZeroWindowPeerFreesItsSlot(t *testing.T) {
	srv, _, run := statusDetailDispatchFixture(t, &fakeRunner{})
	srv.runEvents.write = 50 * time.Millisecond
	dispatchOnce(srv, run)
	ts := newH2EventsServer(t, srv)
	addr := ts.Listener.Addr().String()

	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if p := conn.ConnectionState().NegotiatedProtocol; p != "h2" {
		t.Fatalf("negotiated %q, want h2", p)
	}
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0}); err != nil {
		t.Fatal(err)
	}
	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	for _, f := range [][2]string{
		{":method", http.MethodGet}, {":scheme", "https"}, {":authority", addr},
		{":path", "/api/v1/runs/" + run.ID.String() + "/events"}, {"authorization", "Bearer " + adminToken},
	} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	answered := make(chan struct{})
	go func() { // read and drop every frame; never grant a window
		for once := false; ; {
			f, err := fr.ReadFrame()
			if err != nil {
				return
			}
			if h, ok := f.(*http2.HeadersFrame); ok && h.StreamID == 1 && !once {
				once = true
				close(answered)
			}
		}
	}()
	select {
	case <-answered: // the handler holds its slot and is flushing its first batch
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never answered")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.runEvents.mu.Lock()
		n := len(srv.runEvents.streams)
		srv.runEvents.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a peer that grants no window kept its stream slot: the flush never gave up")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The write deadline covers a batch only: an HTTP/2 stream that waits longer
// than it for the next event must still be open when that event comes.
func TestRunEvents_IdleHTTP2StreamOutlivesTheWriteDeadline(t *testing.T) {
	srv, st, run := statusDetailDispatchFixture(t, &fakeRunner{})
	srv.runEvents.write, srv.runEvents.beat = 20*time.Millisecond, 300*time.Millisecond
	dispatchOnce(srv, run)
	ts := newH2EventsServer(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/runs/"+run.ID.String()+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
		t.Fatalf("GET events: %d over %s, want 200 over HTTP/2", resp.StatusCode, resp.Proto)
	}
	held := &runEventStream{t: t, resp: resp, sc: bufio.NewScanner(resp.Body)}
	held.expect(
		client.RunEvent{ID: 1, Type: client.RunEventProvisioning},
		client.RunEvent{ID: 2, Type: client.RunEventReady},
	)
	st.dispatchTestStore.mu.Lock()
	st.state = types.RunStopped // seen at the next beat, well after the write deadline
	st.dispatchTestStore.mu.Unlock()
	held.expect(client.RunEvent{ID: 3, Type: client.RunEventEnded, State: types.RunStopped})
	held.expectClosed()
}
