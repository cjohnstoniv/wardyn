// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const onceHost = "once.example.test"

// onceProxy is a proxy for a run whose first use of a host asks a person (deny_with_review), with onceHost's once
// approval id decided and not yet used, dialling with dial. raises counts the approvals it raises after that.
func onceProxy(t *testing.T, id uuid.UUID, dial func(context.Context, string, string) (net.Conn, error)) (*Proxy, *atomic.Int32) {
	t.Helper()
	var raises atomic.Int32
	cp := approvalCPStub(apState(types.ApprovalPending), &raises, nil)
	t.Cleanup(cp.Close)
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	seed(ap, approvalHostKey(onceHost), hostApproval{state: apApproved, approvalID: id, scope: types.ScopeOnce, lastPoll: time.Now()})
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"unrelated.test"}, FirstUseApproval: types.FirstUseDenyWithReview}),
		Approval: ap,
		Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
		Resolver: publicResolver{},
		Dial:     dial,
	})
	return p, &raises
}

// dialFailingFirst refuses its first n dials, as a host that is down does, and connects every later one to addr.
func dialFailingFirst(n int32, addr string) func(context.Context, string, string) (net.Conn, error) {
	var calls atomic.Int32
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		if calls.Add(1) <= n {
			return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
}

// upstreamThat runs a loopback destination that hands each connection to serve and then closes it.
func upstreamThat(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				serve(c)
			}()
		}
	}()
	return ln.Addr().String()
}

// quietUpstream reads each connection until the proxy closes it, answering nothing.
func quietUpstream(t *testing.T) string {
	return upstreamThat(t, func(c net.Conn) { _, _ = io.Copy(io.Discard, c) })
}

// TestOnceRefund_AFailedDialKeepsTheGrantForTheRetry: a CONNECT released by a once approval whose dial fails gives
// the approval back. Its row names the approval and says it was not spent, the retry is released by the same
// approval with nothing raised, and the request after that asks again.
func TestOnceRefund_AFailedDialKeepsTheGrantForTheRetry(t *testing.T) {
	id := uuid.New()
	p, raises := onceProxy(t, id, dialFailingFirst(1, quietUpstream(t)))
	proxyURL, done := serveTunnelProxy(t, p)

	conn, resp := connectThrough(t, proxyURL, onceHost+":443")
	_ = conn.Close()
	if !strings.HasPrefix(resp, "HTTP/1.1 502") {
		t.Fatalf("first CONNECT answer = %q, want the 502 of a failed dial", resp)
	}
	var failed []egress.DecisionLog
	for _, d := range awaitDecision(t, p, done) {
		if d.RuleSource == "builtin:dial-failed" {
			failed = append(failed, d)
		}
	}
	if len(failed) != 1 || failed[0].ApprovalID == nil || *failed[0].ApprovalID != id || !strings.HasSuffix(failed[0].Cause, onceRefundNote) {
		t.Fatalf("dial-failed rows = %+v, want one naming approval %s whose cause ends %q", failed, id, onceRefundNote)
	}

	conn, resp = connectThrough(t, proxyURL, onceHost+":443")
	_ = conn.Close()
	if !strings.HasPrefix(resp, "HTTP/1.1 200") {
		t.Fatalf("retry CONNECT answer = %q, want the 200 of the grant given back", resp)
	}
	if rows := awaitDecision(t, p, done); len(rows) == 0 || rows[0].Decision != egress.Allow || rows[0].RuleSource != "approval:"+id.String() {
		t.Fatalf("retry rows = %+v, want an allow released by approval %s", rows, id)
	}
	if n := raises.Load(); n != 0 {
		t.Fatalf("approvals raised by the retry = %d, want 0", n)
	}

	conn, resp = connectThrough(t, proxyURL, onceHost+":443")
	_ = conn.Close()
	awaitDecision(t, p, done)
	if strings.HasPrefix(resp, "HTTP/1.1 200") || raises.Load() != 1 {
		t.Fatalf("third CONNECT answer = %q with %d approvals raised, want a refusal and one new approval", resp, raises.Load())
	}
}

// TestOnceRefund_AnUpstreamThatAnsweredNothingKeepsTheGrant: a tunnel released by a once approval, after the
// sandbox's TLS hello, whose upstream closed with no byte back or answered in plaintext that the proxy refused and
// did not relay, gives the approval back, and the retry is released by it with nothing raised. The rest spend it:
// the same close after plain bytes (a host may well close on bytes it does not speak), a TLS alert (relayed to the
// sandbox, so an answer), and an upstream that wrote before the sandbox sent anything.
func TestOnceRefund_AnUpstreamThatAnsweredNothingKeepsTheGrant(t *testing.T) {
	hello := func(c net.Conn) bool { _, err := io.ReadFull(c, make([]byte, len(fakeClientHello))); return err == nil }
	hangUp := func(c net.Conn) { _, _ = c.Read(make([]byte, 1)) }
	for _, c := range []struct {
		name   string
		serve  func(net.Conn)
		opener []byte
		kept   bool
	}{
		{"closed after a TLS hello", hangUp, fakeClientHello, true},
		{"answered a TLS hello in plaintext", func(c net.Conn) {
			if hello(c) {
				_, _ = io.WriteString(c, hostile503)
			}
		}, fakeClientHello, true},
		{"closed after plain bytes", hangUp, []byte("hello\r\n"), false},
		{"answered a TLS hello with an alert", func(c net.Conn) {
			if hello(c) {
				_, _ = c.Write(plaintextAlert)
			}
		}, fakeClientHello, false},
		{"wrote before the sandbox spoke", func(c net.Conn) { _, _ = io.WriteString(c, hostile503) }, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := uuid.New()
			p, raises := onceProxy(t, id, redirectDial(upstreamThat(t, c.serve)))
			proxyURL, done := serveTunnelProxy(t, p)

			conn, resp := connectThrough(t, proxyURL, onceHost+":443")
			if !strings.HasPrefix(resp, "HTTP/1.1 200") {
				t.Fatalf("CONNECT answer = %q, want the once grant's 200", resp)
			}
			if len(c.opener) > 0 {
				if _, err := conn.Write(c.opener); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			_ = readToEnd(t, conn)
			_ = conn.Close()
			rows := tunnelFailedRows(awaitDecision(t, p, done))
			if len(rows) != 1 {
				t.Fatalf("%s rows = %d, want 1", tunnelFailedSource, len(rows))
			}
			if got := strings.HasSuffix(rows[0].Cause, onceRefundNote); got != c.kept {
				t.Fatalf("cause = %q: ends with the not-spent note = %v, want %v", rows[0].Cause, got, c.kept)
			}

			conn, resp = connectThrough(t, proxyURL, onceHost+":443")
			_ = conn.Close()
			awaitDecision(t, p, done)
			if got := strings.HasPrefix(resp, "HTTP/1.1 200"); got != c.kept {
				t.Fatalf("retry CONNECT answer = %q: a 200 = %v, want %v", resp, got, c.kept)
			}
			wantRaises := int32(1)
			if c.kept {
				wantRaises = 0
			}
			if n := raises.Load(); n != wantRaises {
				t.Fatalf("approvals raised by the retry = %d, want %d", n, wantRaises)
			}
		})
	}
}

// TestOnceRefund_ASandboxThatClosesFirstSpendsTheGrant: a sandbox that sends its hello and closes before any reply
// controls that close itself, so the once approval stays spent and the retry asks again.
func TestOnceRefund_ASandboxThatClosesFirstSpendsTheGrant(t *testing.T) {
	id := uuid.New()
	p, raises := onceProxy(t, id, redirectDial(quietUpstream(t)))
	proxyURL, done := serveTunnelProxy(t, p)

	conn, resp := connectThrough(t, proxyURL, onceHost+":443")
	if !strings.HasPrefix(resp, "HTTP/1.1 200") {
		t.Fatalf("CONNECT answer = %q, want the once grant's 200", resp)
	}
	if _, err := conn.Write(fakeClientHello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	_ = conn.Close()
	rows := tunnelFailedRows(awaitDecision(t, p, done))
	if len(rows) != 1 || strings.Contains(rows[0].Cause, onceRefundNote) {
		t.Fatalf("%s rows = %+v, want one without the not-spent note", tunnelFailedSource, rows)
	}

	conn, resp = connectThrough(t, proxyURL, onceHost+":443")
	_ = conn.Close()
	awaitDecision(t, p, done)
	if strings.HasPrefix(resp, "HTTP/1.1 200") || raises.Load() != 1 {
		t.Fatalf("retry CONNECT answer = %q with %d approvals raised, want a refusal and one new approval", resp, raises.Load())
	}
}

// closeHooks runs onCloseWrite when tunnel half-closes the connection and onClose when it closes it.
type closeHooks struct {
	net.Conn
	onCloseWrite, onClose func()
}

func (c *closeHooks) CloseWrite() error {
	if c.onCloseWrite != nil {
		c.onCloseWrite()
	}
	return nil
}

func (c *closeHooks) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	return c.Conn.Close()
}

// TestOnceRefund_TheSandboxsCloseIsReportedAsTheFirst: tunnel reports the sandbox side when the sandbox closed
// first, though the half-close that passes its close on makes the upstream hang up, and the sandbox side's copy
// reports only after the upstream side's has. Taken from the order of the reports, the row said the upstream closed
// without answering and the once grant was given back.
func TestOnceRefund_TheSandboxsCloseIsReportedAsTheFirst(t *testing.T) {
	client, sandbox := net.Pipe()
	upstream, peer := net.Pipe()
	closed := make(chan struct{})
	a := &closeHooks{Conn: client, onClose: func() { close(closed) }}
	b := &closeHooks{Conn: upstream, onCloseWrite: func() {
		_ = peer.Close()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
		}
	}}
	_ = sandbox.Close()
	if first, err := tunnel(a, b); first != net.Conn(a) || err != nil {
		t.Fatalf("tunnel first = %v, err = %v: want the sandbox side that closed first, with no error", first, err)
	}
}

// TestOnceRefund_ARefusalRecordedAfterTheRowSpendsTheGrant: the row is classified before the guard records its
// refusal. The refund follows the row, which says the client closed first.
func TestOnceRefund_ARefusalRecordedAfterTheRowSpendsTheGrant(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	w := newTunnelWatch(a)
	w.client.Store(clientTLS)
	_, refusal := w.failureClass(false, nil)
	w.refused.Store(&tunnelRefusal{armed: true})
	if w.answeredNothing(false, refusal) {
		t.Fatalf("a row with reason %s was given a refund", refusal.Reason)
	}
}
