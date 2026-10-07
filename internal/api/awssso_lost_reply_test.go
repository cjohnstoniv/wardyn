// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A renewal AWS accepted whose reply never came back: the retry meets
// invalid_grant and the sign-in is removed, as before; the two rows that
// describe the loss now carry after_lost_reply. A request that was never sent
// in full, or that was answered by a page, is not a lost reply.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// lostReplyRun renews the stored, expired session as dispatch does and
// returns the refusal and the two rows that describe its end.
func lostReplyRun(t *testing.T, srv *Server, blob awsSSOBlob) (failure string, refresh, deleted map[string]any) {
	t.Helper()
	storeSSOBlobFor(t, srv, createRenewalOwner, blob)
	_, failure = srv.refreshAWSSSOBlob(context.Background(), createRenewalScope(), blob)
	for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
		var data map[string]any
		_ = json.Unmarshal(ev.Data, &data)
		switch {
		case ev.Action == "harness.credential.refresh" && ev.Outcome == "failure":
			refresh = data
		case ev.Action == "credential.expired.delete":
			deleted = data
		}
	}
	if refresh == nil || deleted == nil {
		t.Fatalf("refresh row = %v, delete row = %v, want both", refresh, deleted)
	}
	if _, found := createRenewalStored(t, srv); found {
		t.Error("the refused sign-in is still stored")
	}
	return failure, refresh, deleted
}

// rotatingOIDC honours each refresh token once, as AWS does, and answers the
// first call through first instead.
func rotatingOIDC(t *testing.T, first func(w http.ResponseWriter)) {
	t.Helper()
	var mu sync.Mutex
	spent := map[string]bool{}
	fakeOIDC(t, func(w http.ResponseWriter, body map[string]string, call int) {
		mu.Lock()
		again := spent[body["refreshToken"]]
		spent[body["refreshToken"]] = true
		mu.Unlock()
		switch {
		case again:
			invalidGrant(w)
		case call == 1:
			first(w)
		default:
			renewedPair(w)
		}
	})
}

// The first request is read in full and its token replaced, and the reply is
// dropped: today's outcome (the retry ends invalid_grant, the sign-in is
// deleted), and both rows carry the marker.
func TestLostReply_DroppedReplyMarksBothRows(t *testing.T) {
	srv := createRenewalFixture(t)
	rotatingOIDC(t, func(w http.ResponseWriter) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	failure, refresh, deleted := lostReplyRun(t, srv, createRenewalBlob())
	if failure != awsSSORefreshSpentSentence || refresh["spent"] != true || refresh["attempts"] != float64(2) {
		t.Errorf("failure = %q, refresh row = %v, want the spent sentence after two attempts", failure, refresh)
	}
	if refresh["after_lost_reply"] != true || deleted["after_lost_reply"] != true || deleted["reason"] != "invalid_grant" {
		t.Errorf("refresh row = %v, delete row = %v, want after_lost_reply on both", refresh, deleted)
	}
}

// A page the proxy wrote itself is a reply: no marker.
func TestLostReply_ProxyPageSetsNoMarker(t *testing.T) {
	srv := createRenewalFixture(t)
	rotatingOIDC(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html><body>502 Bad Gateway</body></html>")
	})
	failure, refresh, deleted := lostReplyRun(t, srv, createRenewalBlob())
	if failure != awsSSORefreshSpentSentence {
		t.Errorf("failure = %q, want the spent sentence", failure)
	}
	if _, ok := refresh["after_lost_reply"]; ok {
		t.Errorf("refresh row = %v, want no marker after a proxy's page", refresh)
	}
	if _, ok := deleted["after_lost_reply"]; ok {
		t.Errorf("delete row = %v, want no marker after a proxy's page", deleted)
	}
}

// A request whose write failed part-way never reached AWS whole: no marker. The
// refresh token is large enough that the body cannot fit in the socket buffers,
// and the endpoint resets the connection without reading it.
func TestLostReply_PartialWriteSetsNoMarker(t *testing.T) {
	srv := createRenewalFixture(t)
	var mu sync.Mutex
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = conn.Close()
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		invalidGrant(w)
	}))
	t.Cleanup(ts.Close)
	prev := awsSSOTokenURL
	awsSSOTokenURL = func(string) string { return ts.URL + "/token" }
	t.Cleanup(func() { awsSSOTokenURL = prev })

	blob := createRenewalBlob()
	blob.RefreshToken = strings.Repeat("r", 32<<20)
	failure, refresh, deleted := lostReplyRun(t, srv, blob)
	if failure != awsSSORefreshSpentSentence || refresh["attempts"] != float64(2) {
		t.Errorf("failure = %q, refresh row = %v, want the spent sentence after two attempts", failure, refresh)
	}
	if _, ok := refresh["after_lost_reply"]; ok {
		t.Errorf("refresh row = %v, want no marker after a failed write", refresh)
	}
	if _, ok := deleted["after_lost_reply"]; ok {
		t.Errorf("delete row = %v, want no marker after a failed write", deleted)
	}
}
