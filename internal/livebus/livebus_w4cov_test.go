// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package livebus

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var w4CovRun = uuid.MustParse("cccccccc-0000-0000-0000-000000000001")

func TestW4CovNewCarriesItsOrigin(t *testing.T) {
	b := New(nil, "replica-a")
	if b.Origin() != "replica-a" {
		t.Fatalf("Origin = %q", b.Origin())
	}
	if b.handlers == nil {
		t.Fatal("New left the handler map nil: Handle would panic")
	}
}

func TestW4CovDispatchCallsOnlyTheKindsHandlersInOrder(t *testing.T) {
	b := New(nil, "replica-a")
	var got []string
	var heard Message
	b.Handle(KindRunKill, func(m Message) { got = append(got, "first"); heard = m })
	b.Handle(KindRunKill, func(Message) { got = append(got, "second") })
	b.Handle(KindRunEvent, func(Message) { got = append(got, "other-kind") })

	payload, err := json.Marshal(Message{Kind: KindRunKill, Run: w4CovRun, Origin: "replica-b", Data: json.RawMessage(`{"why":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	b.dispatch(context.Background(), string(payload))

	if strings.Join(got, ",") != "first,second" {
		t.Errorf("handlers called = %v, want the two kill handlers in registration order", got)
	}
	if heard.Kind != KindRunKill || heard.Run != w4CovRun || heard.Origin != "replica-b" || string(heard.Data) != `{"why":"x"}` {
		t.Errorf("handler heard %+v", heard)
	}
}

func TestW4CovDispatchDropsANoticeThatDoesNotParse(t *testing.T) {
	b := New(nil, "replica-a")
	b.Handle(KindRunKill, func(m Message) { t.Errorf("a handler ran for an unparseable notice: %+v", m) })
	b.dispatch(context.Background(), "{not json")
	b.dispatch(context.Background(), "")
}

func TestW4CovDispatchOfAKindWithNoHandlerIsQuiet(t *testing.T) {
	b := New(nil, "replica-a")
	b.Handle(KindRunKill, func(m Message) { t.Errorf("the kill handler ran for %s", m.Kind) })
	b.dispatch(context.Background(), `{"k":"attach_free","r":"cccccccc-0000-0000-0000-000000000001","o":"x"}`)
}

func TestW4CovPublishRefusesBeforeTouchingPostgres(t *testing.T) {
	b := New(nil, "replica-a") // a nil pool panics if a refused notice reached it
	ctx := context.Background()

	err := b.Publish(ctx, KindRunEvent, w4CovRun, make(chan int))
	if err == nil || !strings.Contains(err.Error(), "livebus: encode a notice") {
		t.Errorf("an unencodable payload: %v", err)
	}

	err = b.Publish(ctx, KindRunEvent, w4CovRun, strings.Repeat("x", maxPayload+1))
	if err == nil || !strings.Contains(err.Error(), "too large") || !strings.Contains(err.Error(), KindRunEvent) {
		t.Errorf("an oversize notice: %v", err)
	}
}

func TestW4CovListenerFailureIsLogged(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db?connect_timeout=1") // lazy: no dial yet
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	New(pool, "replica-a").listenOnce(context.Background())
	if out := logs.String(); !strings.Contains(out, "listener failed") || !strings.Contains(out, "step=connect") {
		t.Errorf("log = %q, want the failed connect named", out)
	}
}

// A listener that keeps failing the same way logs it once, not on every retry.
func TestListenerRepeatedFailureIsLoggedOnce(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	b := New(pool, "replica-a")
	for range 3 {
		b.listenOnce(context.Background())
	}
	if n := strings.Count(logs.String(), "listener failed"); n != 1 {
		t.Errorf("logged the same failure %d times over three retries, want once:\n%s", n, logs.String())
	}
}
