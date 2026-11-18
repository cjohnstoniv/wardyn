// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

// TestOnceRefund_TheRetryRowNamesTheApprovalItWentOn: the request that goes ahead on a once approval given back
// carries its approval_id in its row, whatever comes of it: a retry that connects, and a retry whose dial fails
// again (the approval is spent, no second give-back). A request with no approval has none, failing or not.
func TestOnceRefund_TheRetryRowNamesTheApprovalItWentOn(t *testing.T) {
	id := uuid.New()
	for _, c := range []struct {
		name      string
		failFirst int32
		wantRetry string
	}{
		{"the retry connects", 1, "approval:" + id.String()},
		{"the retry fails again", 2, "builtin:dial-failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, _ := onceProxy(t, id, dialFailingFirst(c.failFirst, quietUpstream(t)))
			proxyURL, done := serveTunnelProxy(t, p)

			conn, _ := connectThrough(t, proxyURL, onceHost+":443")
			_ = conn.Close()
			first := awaitDecision(t, p, done)
			if len(first) == 0 || first[len(first)-1].ApprovalID == nil || *first[len(first)-1].ApprovalID != id {
				t.Fatalf("first try rows = %+v, want the failure to name approval %s", first, id)
			}

			conn, _ = connectThrough(t, proxyURL, onceHost+":443")
			_ = conn.Close()
			var retry *egress.DecisionLog
			for _, d := range awaitDecision(t, p, done) {
				if d.RuleSource == c.wantRetry {
					retry = &d
				}
			}
			if retry == nil || retry.ApprovalID == nil || *retry.ApprovalID != id {
				t.Fatalf("retry row (rule_source %s) = %+v, want approval_id %s", c.wantRetry, retry, id)
			}
		})
	}

	t.Run("a request with no approval has no approval_id", func(t *testing.T) {
		p := newProxy(Options{
			RunID:    uuid.New(),
			Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{onceHost}}),
			Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
			Resolver: publicResolver{},
			Dial:     dialFailingFirst(1, quietUpstream(t)),
		})
		proxyURL, done := serveTunnelProxy(t, p)
		conn, _ := connectThrough(t, proxyURL, onceHost+":443")
		_ = conn.Close()
		var failed []egress.DecisionLog
		for _, d := range awaitDecision(t, p, done) {
			if d.RuleSource == "builtin:dial-failed" {
				failed = append(failed, d)
			}
		}
		if len(failed) != 1 || failed[0].ApprovalID != nil {
			t.Fatalf("dial-failed rows = %+v, want one with approval_id null", failed)
		}
	})
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

// resetOnWrite is a connection whose every write fails as a reset does, with its read side still open.
type resetOnWrite struct{ net.Conn }

func (resetOnWrite) Write([]byte) (int, error) { return 0, syscall.ECONNRESET }

// TestOnceRefund_AWriteThatFailsNamesTheSideItWroteTo: an upstream reset that lands while the proxy is writing the
// client's bytes ends the client-to-upstream copy on a write error. tunnel reports the upstream, which broke, and
// not the client, whose connection is still open: the row would otherwise say the client closed before any reply.
func TestOnceRefund_AWriteThatFailsNamesTheSideItWroteTo(t *testing.T) {
	client, sandbox := net.Pipe()
	upstream, peer := net.Pipe()
	t.Cleanup(func() { _ = sandbox.Close(); _ = peer.Close() })
	b := resetOnWrite{upstream}
	go func() { _, _ = sandbox.Write([]byte("hello")) }()
	first, err := tunnel(client, b)
	if first != net.Conn(b) || !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("tunnel first = %v, err = %v: want the upstream whose write failed, with its reset", first, err)
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

// The retry that went ahead on the given-back grant dies again; its tunnel-failed row must name it.
func TestOnceRefund_TheTunnelRowOfAGivenBackUseNamesTheApproval(t *testing.T) {
	hello := func(c net.Conn) bool { _, err := io.ReadFull(c, make([]byte, len(fakeClientHello))); return err == nil }
	for _, c := range []struct {
		name   string
		second func(net.Conn)
	}{
		{"retry answered with an alert", func(c net.Conn) {
			if hello(c) {
				_, _ = c.Write(plaintextAlert)
			}
		}},
		{"retry closed again after hello", func(c net.Conn) { _, _ = c.Read(make([]byte, 1)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var n atomic.Int32
			up := upstreamThat(t, func(conn net.Conn) {
				if n.Add(1) == 1 {
					_, _ = conn.Read(make([]byte, 1))
					return
				}
				c.second(conn)
			})
			id := uuid.New()
			p, _ := onceProxy(t, id, redirectDial(up))
			proxyURL, done := serveTunnelProxy(t, p)
			for i := 0; i < 2; i++ {
				conn, resp := connectThrough(t, proxyURL, onceHost+":443")
				if !strings.HasPrefix(resp, "HTTP/1.1 200") {
					t.Fatalf("try %d CONNECT = %q", i, resp)
				}
				_, _ = conn.Write(fakeClientHello)
				_ = readToEnd(t, conn)
				_ = conn.Close()
				rows := tunnelFailedRows(awaitDecision(t, p, done))
				if len(rows) != 1 || rows[0].ApprovalID == nil || *rows[0].ApprovalID != id {
					t.Fatalf("try %d tunnel-failed rows = %+v, want one naming %s", i, rows, id)
				}
				if note := strings.HasSuffix(rows[0].Cause, onceRefundNote); note != (i == 0) {
					t.Fatalf("try %d note = %v", i, note)
				}
			}
		})
	}
}

// A third request to the host lands while the given-back retry is still dialling.
func TestOnceRefund_ARequestToTheHostMidRetryDoesNotLoseTheName(t *testing.T) {
	var calls atomic.Int32
	second, release := make(chan struct{}), make(chan struct{})
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		if calls.Add(1) == 2 {
			close(second)
			<-release
		}
		return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
	}
	id := uuid.New()
	p, raises := onceProxy(t, id, dial)
	proxyURL, done := serveTunnelProxy(t, p)
	conn, _ := connectThrough(t, proxyURL, onceHost+":443")
	_ = conn.Close()
	awaitDecision(t, p, done)

	r2 := make(chan string, 1)
	go func() {
		c, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
		if err != nil {
			r2 <- err.Error()
			return
		}
		defer c.Close()
		_, _ = io.WriteString(c, "CONNECT "+onceHost+":443 HTTP/1.1\r\nHost: "+onceHost+":443\r\n\r\n")
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		b := make([]byte, 256)
		k, _ := c.Read(b)
		r2 <- string(b[:k])
	}()
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("retry never dialled")
	}
	conn, resp3 := connectThrough(t, proxyURL, "other-port."+onceHost+":443")
	_ = conn.Close()
	_ = resp3
	conn, resp3 = connectThrough(t, proxyURL, onceHost+":8443") // same approval host key, other port
	_ = conn.Close()
	r3rows := awaitDecision(t, p, done)
	awaitDecision(t, p, done)
	close(release)
	resp2 := <-r2
	rows := awaitDecision(t, p, done)
	t.Logf("racer resp=%q rows=%+v raises=%d", strings.SplitN(resp3, "\r\n", 2)[0], r3rows, raises.Load())
	var failed []egress.DecisionLog
	for _, d := range rows {
		if d.RuleSource == "builtin:dial-failed" {
			failed = append(failed, d)
		}
	}
	t.Logf("retry resp=%q", strings.SplitN(resp2, "\r\n", 2)[0])
	if len(failed) != 1 {
		t.Fatalf("retry dial-failed rows = %+v", rows)
	}
	if failed[0].ApprovalID == nil {
		t.Errorf("the retry went ahead on given-back approval %s but its dial-failed row has approval_id null (a request to the same host resolved during its dial)", id)
	}
}

// A first use (not given back) spent by a TLS alert leaves its tunnel-failed row with approval_id null.
func TestOnceRefund_AFirstUseSpentByAnAlertKeepsItsRowNull(t *testing.T) {
	hello := func(c net.Conn) bool { _, err := io.ReadFull(c, make([]byte, len(fakeClientHello))); return err == nil }
	id := uuid.New()
	p, _ := onceProxy(t, id, redirectDial(upstreamThat(t, func(c net.Conn) {
		if hello(c) {
			_, _ = c.Write(plaintextAlert)
		}
	})))
	proxyURL, done := serveTunnelProxy(t, p)
	conn, resp := connectThrough(t, proxyURL, onceHost+":443")
	if !strings.HasPrefix(resp, "HTTP/1.1 200") {
		t.Fatalf("CONNECT = %q", resp)
	}
	_, _ = conn.Write(fakeClientHello)
	_ = readToEnd(t, conn)
	_ = conn.Close()
	rows := tunnelFailedRows(awaitDecision(t, p, done))
	if len(rows) != 1 || rows[0].ApprovalID != nil {
		t.Fatalf("first-use spent rows = %+v, want one with approval_id null", rows)
	}
}

// The terminated tunnel's client handshake row names the approval only for a given-back use.
func TestOnceRefund_TheMITMHandshakeRowNamesAGivenBackUse(t *testing.T) {
	id := uuid.New()
	for _, c := range []struct {
		name string
		seen *egress.DecisionLog
		want bool
	}{
		{"given-back use", &egress.DecisionLog{ApprovalID: &id, GivenBackUse: true}, true},
		{"first use", &egress.DecisionLog{ApprovalID: &id}, false},
		{"no seen (via_org)", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, _, _ := mitmProxy(t, "alert", captureUpstream(t, true, "ok").srv)
			done := make(chan struct{}, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p.mitmConnect(w, r, anthropicHost, 443, c.seen)
				done <- struct{}{}
			}))
			t.Cleanup(srv.Close)
			conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = conn.Write([]byte("CONNECT " + anthropicHost + ":443 HTTP/1.1\r\nHost: " + anthropicHost + ":443\r\n\r\n"))
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			b := make([]byte, 512)
			n, _ := conn.Read(b)
			if !strings.HasPrefix(string(b[:n]), "HTTP/1.1 200") {
				t.Fatalf("answer %q", b[:n])
			}
			_, _ = conn.Write([]byte("\x16\x03\x01\x00\x05" + clientMarker))
			_ = conn.Close()
			rows := tunnelFailedRows(awaitDecision(t, p, done))
			if len(rows) != 1 {
				t.Fatalf("rows = %+v", rows)
			}
			if got := rows[0].ApprovalID != nil && *rows[0].ApprovalID == id; got != c.want {
				t.Fatalf("approval_id named = %v, want %v (row %+v)", got, c.want, rows[0])
			}
			if strings.HasSuffix(rows[0].Cause, onceRefundNote) {
				t.Fatalf("handshake row carries the not-spent note")
			}
		})
	}
}
