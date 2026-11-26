// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeServer accepts connections on a loopback port and serves each as a Postgres server that asks for no
// password: it answers every simple-protocol statement (a SET) as done, and hangs up at the first
// extended-protocol one (a lock try). Each session's statements, in order, are sent on stmts when it ends; a
// connection that starts no session (the cancel request pgx sends after a failed statement) sends nothing.
func fakeServer(t *testing.T) (addr string, stmts <-chan []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	out := make(chan []string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if got := serveFake(conn); got != nil {
					out <- got
				}
			}()
		}
	}()
	return ln.Addr().String(), out
}

func serveFake(conn net.Conn) []string {
	be := pgproto3.NewBackend(conn, conn)
	for started := false; !started; {
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			return nil
		}
		switch msg.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			if _, err := conn.Write([]byte("N")); err != nil {
				return nil
			}
		case *pgproto3.StartupMessage:
			started = true
		default: // a cancel request, which pgx dials after a session fails: not a session
			return nil
		}
	}
	be.Send(&pgproto3.AuthenticationOk{})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := be.Flush(); err != nil {
		return nil
	}
	seen := []string{}
	for {
		msg, err := be.Receive()
		if err != nil {
			return seen
		}
		switch m := msg.(type) {
		case *pgproto3.Query:
			seen = append(seen, m.String)
			be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SET")})
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err := be.Flush(); err != nil {
				return seen
			}
		case *pgproto3.Parse:
			return append(seen, m.Query)
		case *pgproto3.Terminate:
			return seen
		}
	}
}

// TestLifetimeLockDialSetsNoKeepalive: a process-lifetime lock (the single-instance lock, the gateway key
// rotation lock) sets none of the lock connections' keepalives: the first statement its session sends is the
// lock itself. The election dial (dialDedicated), on the same server, sends every one of them first. The
// keepalive dialer's socket options are not visible to a server, so the SETs are what tells the two apart.
func TestLifetimeLockDialSetsNoKeepalive(t *testing.T) {
	addr, stmts := fakeServer(t)
	cfg, err := pgxpool.ParseConfig(fmt.Sprintf("postgres://wardyn@%s/wardyn?sslmode=disable&connect_timeout=5", addr))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session := func(what string) []string {
		t.Helper()
		select {
		case got := <-stmts:
			return got
		case <-ctx.Done():
			t.Fatalf("the fake server saw no session for %s", what)
			return nil
		}
	}

	if _, _, ok, err := TryAdvisoryLockDedicated(ctx, pool, 42); ok || err == nil {
		t.Fatalf("TryAdvisoryLockDedicated against a server that hangs up at the lock = ok %v, err %v; want an error", ok, err)
	}
	got := session("the lifetime lock")
	if len(got) == 0 || !strings.Contains(got[0], "pg_try_advisory_lock") {
		t.Fatalf("the lifetime lock's statements = %q; want the lock try first", got)
	}
	for _, s := range got {
		if strings.Contains(s, "tcp_") {
			t.Fatalf("the lifetime lock's session sent %q; want no keepalive setting", s)
		}
	}

	conn, err := dialDedicated(ctx, pool)
	if err != nil {
		t.Fatalf("the election dial: %v", err)
	}
	conn.Close(ctx) //nolint:errcheck // the fake server ends the session either way
	got = session("the election dial")
	for _, p := range lockKeepalives {
		if want := "SET " + p.name + " = " + p.value; !slices.Contains(got, want) {
			t.Fatalf("the election dial's statements = %q; want %q among them, or this test can no longer tell the two dials apart", got, want)
		}
	}
}
