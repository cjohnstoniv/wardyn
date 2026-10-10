// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestHubRefusedStateRestartsPendingBeforeReady(t *testing.T) {
	for _, realWire := range []bool{false, true} {
		name := "loopback"
		if realWire {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) { testHubStateRetry(t, realWire) })
	}
}

func testHubStateRetry(t *testing.T, realWire bool) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), Owner: "owner", PublicKey: pub, State: types.RunnerClaimed}
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	action := runnerwire.PendingAction{ID: uuid.New(), RunID: uuid.New(), Kind: types.RunnerActionStopProxy}
	refused := errors.New("state reconciliation queued proxy containment")
	var queued, applied atomic.Bool
	var states, pendingReads atomic.Int32
	config := HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				Pending: func(context.Context) ([]runnerwire.PendingAction, error) {
					pendingReads.Add(1)
					if queued.Load() {
						return []runnerwire.PendingAction{action}, nil
					}
					return nil, nil
				},
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage: func(context.Context, string, runnerwire.StatePage) error {
					if states.Add(1) == 1 {
						// H3 persists the real queue before returning this refusal.
						queued.Store(true)
						return refused
					}
					if !applied.Load() {
						return errors.New("STATE preceded durable pending result")
					}
					return nil
				},
				Event: func(_ context.Context, event runnerwire.Event) error {
					var result runnerwire.ActionResult
					if event.Kind != runnerwire.EventActionResult || runnerwire.Unmarshal(event.Data, &result) != nil || result.ID != action.ID || result.Outcome != "applied" {
						return errors.New("unexpected containment result")
					}
					applied.Store(true)
					return nil
				},
			}, nil
		},
	}
	var hub *Hub
	hubDone := make(chan error, 2)
	if realWire {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err == nil {
				err = hub.Serve(ctx, runnerwire.NewWebSocket(conn), row.ID)
			}
			hubDone <- err
		}))
		defer server.Close()
		config.OrgURL = server.URL
	}
	row.OrgURLSHA256 = federation.OrgURLSHA256(config.OrgURL)
	hub = NewHub(ctx, config)
	defer hub.Close()
	dial := func() runnerwire.Conn {
		if realWire {
			conn, err := runnerwire.DialWebSocket(ctx, config.OrgURL, row.ID.String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			return conn
		}
		org, run := runnerwire.Loopback()
		go func() { hubDone <- hub.Serve(ctx, org, row.ID) }()
		return run
	}
	first := dial()
	defer first.Close()
	ok, err := runnerwire.AuthenticateOrg(ctx, first, row.ID.String(), config.OrgURL, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := first.ReadFrame(ctx)
	if err != nil || initial.Type != runnerwire.TypePending {
		t.Fatal("missing initial PENDING", err)
	}
	if err := first.WriteFrame(ctx, runnerwire.Frame{Type: runnerwire.TypeState, Seq: 1, Payload: emptyStatePage(t)}); err != nil {
		t.Fatal(err)
	}
	// READY can already be queued locally when the org refuses STATE.
	_ = first.WriteFrame(ctx, runnerwire.Frame{Type: runnerwire.TypeReady, Seq: 2})
	if err := awaitValue(t, ctx, hubDone); !errors.Is(err, refused) {
		t.Fatal("STATE refusal did not close admission", err)
	}
	if link.Online() || link.Call(ctx, "probe", nil, nil) == nil {
		t.Fatal("refused STATE allowed dispatch")
	}
	second := dial()
	defer second.Close()
	fresh, err := runnerwire.AuthenticateOrg(ctx, second, row.ID.String(), config.OrgURL, key, &runnerwire.Resume{SessionID: ok.SessionID, LastSeq: initial.Seq})
	if err != nil || fresh.Resumed || fresh.SessionID == ok.SessionID {
		t.Fatal("refused STATE resumed instead of draining the new queue", fresh, err)
	}
	frame, err := second.ReadFrame(ctx)
	var actions []runnerwire.PendingAction
	if err != nil || frame.Type != runnerwire.TypePending || runnerwire.Unmarshal(frame.Payload, &actions) != nil || len(actions) != 1 || actions[0].ID != action.ID {
		t.Fatal("fresh PENDING lost the containment request", frame, err)
	}
	result, _ := runnerwire.Marshal(runnerwire.ActionResult{ID: action.ID, Outcome: "applied"})
	event, _ := runnerwire.Marshal(runnerwire.Event{Kind: runnerwire.EventActionResult, Data: result})
	for _, next := range []runnerwire.Frame{{Type: runnerwire.TypeEvent, Seq: 1, Payload: event}, {Type: runnerwire.TypeState, Seq: 2, Payload: emptyStatePage(t)}, {Type: runnerwire.TypeReady, Seq: 3}} {
		if err := second.WriteFrame(ctx, next); err != nil {
			t.Fatal(err)
		}
	}
	waitOnline(t, ctx, link)
	if !applied.Load() || states.Load() != 2 || pendingReads.Load() != 2 {
		t.Fatal("fresh admission bypassed pending containment")
	}
	second.Close()
	awaitValue(t, ctx, hubDone)
}
