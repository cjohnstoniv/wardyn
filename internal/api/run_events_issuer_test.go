// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestRunEventsIssuerPortal_CutBetweenWakeAndDeadline: the cut lands after a batch's wake and before the batch
// sets its write deadline. The batch's SetWriteDeadline is refused, and the event it woke for is never written.
func TestRunEventsIssuerPortal_CutBetweenWakeAndDeadline(t *testing.T) {
	rig := newIssuerUIRig(t, okBackend())
	g := rig.is.grant(rig.principal, uuid.Nil, rig.clock.now().Add(time.Hour))
	via := types.DelegationVia{Delegate: g.DelegateID, Grant: g.ID}
	ta := ticketActor{actorType: types.ActorHuman, principal: rig.principal, via: &via}
	rig.srv.runEvents.beat = 50 * time.Millisecond
	setSupportWatch(t, time.Hour, 200*time.Millisecond)
	var armed, cutSeen atomic.Bool
	rig.srv.runEvents.onBatch = func(c *streamCut) {
		if !armed.Load() {
			return
		}
		rig.revoke()
		for deadline := time.Now().Add(5 * time.Second); !c.isCut() && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		cutSeen.Store(c.isCut())
	}
	router := chi.NewRouter()
	router.Get("/runs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		rig.srv.handleRunEvents(w, r.WithContext(withTicketActor(r.Context(), ta)))
	})
	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/runs/" + rig.run.ID.String() + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("open: %d %s", resp.StatusCode, b)
	}
	first := make([]byte, 64)
	if _, err := resp.Body.Read(first); err != nil { // a keepalive: the stream is open and past its first batch
		t.Fatalf("the stream wrote nothing: %v", err)
	}
	armed.Store(true)
	rig.srv.runEvents.emit(rig.run.ID, client.RunEvent{Type: client.RunEventPulling, At: time.Now()})
	rest, _ := io.ReadAll(resp.Body)
	if !cutSeen.Load() {
		t.Fatal("the hook never saw the cut")
	}
	if strings.Contains(string(first)+string(rest), client.RunEventPulling) {
		t.Fatalf("the event the cut batch woke for was written: %q", rest)
	}
}
