// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeFirstStatement serves one client connection on srv: it answers the startup with no password and
// sends back the text of the first statement the client sends, then hangs up.
func fakeFirstStatement(srv net.Conn) <-chan string {
	got := make(chan string, 1)
	go func() {
		defer srv.Close()
		be := pgproto3.NewBackend(srv, srv)
		for started := false; !started; {
			msg, err := be.ReceiveStartupMessage()
			if err != nil {
				got <- "startup: " + err.Error()
				return
			}
			switch msg.(type) {
			case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
				if _, err := srv.Write([]byte("N")); err != nil {
					got <- "startup: " + err.Error()
					return
				}
			default:
				started = true
			}
		}
		be.Send(&pgproto3.AuthenticationOk{})
		be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := be.Flush(); err != nil {
			got <- "startup: " + err.Error()
			return
		}
		for {
			msg, err := be.Receive()
			if err != nil {
				got <- "receive: " + err.Error()
				return
			}
			switch m := msg.(type) {
			case *pgproto3.Query:
				got <- m.String
				return
			case *pgproto3.Parse:
				got <- m.Query
				return
			}
		}
	}()
	return got
}

// TestLifetimeLockDialSetsNoKeepalive: a process-lifetime lock (the single-instance lock, the gateway key
// rotation lock) dials with the pool's own dialer, not the lock connections' keepalive dialer, and its
// first statement is the lock itself, with no keepalive SET before it. The election dial, by contrast,
// replaces the pool's dialer.
func TestLifetimeLockDialSetsNoKeepalive(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://wardyn@127.0.0.1:1/wardyn?sslmode=disable&connect_timeout=5")
	if err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	cli, srv := net.Pipe()
	first := fakeFirstStatement(srv)
	cfg.ConnConfig.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		if dials.Add(1) > 1 {
			return nil, net.ErrClosed
		}
		return cli, nil
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, ok, err := TryAdvisoryLockDedicated(ctx, pool, 42); ok || err == nil {
		t.Fatalf("TryAdvisoryLockDedicated against a server that hangs up = ok %v, err %v; want an error", ok, err)
	}
	if n := dials.Load(); n != 1 {
		t.Fatalf("the pool's own dialer ran %d times, want 1: the lifetime lock must not swap in the keepalive dialer", n)
	}
	select {
	case sql := <-first:
		if strings.Contains(sql, "tcp_") || !strings.Contains(sql, "pg_try_advisory_lock") {
			t.Fatalf("the lifetime lock's first statement = %q, want the lock try with no keepalive SET before it", sql)
		}
	case <-ctx.Done():
		t.Fatal("the fake server saw no statement")
	}

	// The election dial does replace the dialer: the pool's own one is not called.
	dials.Store(0)
	if conn, err := dialDedicated(ctx, pool); err == nil {
		conn.Close(context.Background()) //nolint:errcheck // nothing listens on port 1
		t.Fatal("the election dial reached port 1")
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("the election dial used the pool's own dialer (%d calls); this test can no longer tell the two apart", n)
	}
}
