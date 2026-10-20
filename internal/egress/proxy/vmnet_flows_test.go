// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// countingUpstream accepts connections, counts them and echoes one line back.
func countingUpstream(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				_, _ = io.WriteString(c, "echo "+line)
			}()
		}
	}()
	return ln.Addr().String(), &n
}

// stagedConnect sends one relay-shaped CONNECT and returns the status with the
// open connection.
func stagedConnect(t *testing.T, proxyAddr, target, flow, stage string) (int, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	hdr := ""
	if flow != "" {
		hdr += vmnetFlowHeader + ": " + flow + "\r\n"
	}
	if stage != "" {
		hdr += vmnetStageHeader + ": " + stage + "\r\n"
	}
	_, _ = fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, target, hdr)
	br := bufio.NewReader(c)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, c, br
}

func rows(t *testing.T, buf *syncBuf) []egress.DecisionLog {
	t.Helper()
	var out []egress.DecisionLog
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var d egress.DecisionLog
		if err := json.Unmarshal([]byte(l), &d); err != nil {
			t.Fatalf("row %q: %v", l, err)
		}
		out = append(out, d)
	}
	return out
}

func postRefusal(t *testing.T, proxyURL string, body string) int {
	t.Helper()
	resp, err := http.Post(proxyURL+routeVMNetRefusals, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

const flowA, flowB = "00112233445566aa", "00112233445566bb"

// TestVMFlowStaging pins the relay contract: an ask decides and parks without
// a dial or a row; the tunnel resumes it, dials once and writes the one allow;
// a refusal consumes the park and writes the one deny; nothing is left as an
// allow row the relay then closed.
func TestVMFlowStaging(t *testing.T) {
	upstream, dials := countingUpstream(t)
	p, buf := newRuleTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"origin.test"}}, upstream, true, "", nil)
	srv := httptest.NewServer(p)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	t.Run("ask parks, tunnel dials once", func(t *testing.T) {
		buf.Reset()
		if st, _, _ := stagedConnect(t, addr, "origin.test:443", flowA, vmnetStageAsk); st != http.StatusNoContent {
			t.Fatalf("ask = %d, want 204", st)
		}
		if dials.Load() != 0 || buf.Len() != 0 {
			t.Fatalf("after the ask: %d dials, rows %q; want none", dials.Load(), buf.String())
		}
		st, c, br := stagedConnect(t, addr, "origin.test:443", flowA, vmnetStageTunnel)
		if st != http.StatusOK {
			t.Fatalf("tunnel = %d", st)
		}
		_, _ = io.WriteString(c, "hello\n")
		if line, _ := br.ReadString('\n'); line != "echo hello\n" {
			t.Fatalf("tunnel carried %q", line)
		}
		rs := waitRows(t, buf, 1)
		if dials.Load() != 1 || len(rs) != 1 || rs[0].Decision != egress.Allow || rs[0].Flow != flowA || rs[0].Request.Method != http.MethodConnect {
			t.Fatalf("%d dials, rows %+v; want one dial and one allow carrying the flow", dials.Load(), rs)
		}
	})
	t.Run("a refusal consumes the park: one deny, no dial", func(t *testing.T) {
		buf.Reset()
		before := dials.Load()
		if st, _, _ := stagedConnect(t, addr, "origin.test:8443", flowB, vmnetStageAsk); st != http.StatusForbidden {
			t.Fatalf("ask :8443 = %d, want 403 (port rule)", st)
		}
		if st, _, _ := stagedConnect(t, addr, "origin.test:443", flowB, vmnetStageAsk); st != http.StatusNoContent {
			t.Fatalf("ask = %d", st)
		}
		if st := postRefusal(t, srv.URL, `{"flow":"`+flowB+`","reason":"sni-mismatch","host":"origin.test","port":443,"detail":"other.test"}`); st != http.StatusNoContent {
			t.Fatalf("refusal = %d", st)
		}
		rs := waitRows(t, buf, 2)
		if len(rs) != 2 || rs[0].RuleSource != ruleSourcePort || rs[0].Flow != flowB {
			t.Fatalf("rows %+v", rs)
		}
		r := rs[1]
		if r.Decision != egress.Deny || r.RuleSource != "vmnet:sni-mismatch" || r.Flow != flowB || r.Request.Host != "origin.test" ||
			r.Request.Port != 443 || !strings.Contains(r.Cause, "(other.test)") {
			t.Fatalf("refusal row %+v", r)
		}
		// The park is gone: a tunnel for the refused flow is decided afresh, not resumed.
		if _, ok := p.vmFlows.claim(flowB, p.now()); ok {
			t.Fatal("the refused flow is still parked")
		}
		if dials.Load() != before {
			t.Fatalf("%d dials for a refused flow", dials.Load()-before)
		}
	})
	t.Run("a tunnel for an unknown flow is evaluated afresh", func(t *testing.T) {
		buf.Reset()
		if st, _, _ := stagedConnect(t, addr, "blocked.test:443", "00000000000000ff", vmnetStageTunnel); st != http.StatusForbidden {
			t.Fatalf("unparked tunnel to a host the policy does not allow = %d, want 403", st)
		}
	})
	t.Run("a parked flow is resumed only for its own host and port", func(t *testing.T) {
		if st, _, _ := stagedConnect(t, addr, "origin.test:443", "0000000000000001", vmnetStageAsk); st != http.StatusNoContent {
			t.Fatal(st)
		}
		if st, _, _ := stagedConnect(t, addr, "blocked.test:443", "0000000000000001", vmnetStageTunnel); st != http.StatusForbidden {
			t.Fatalf("tunnel to another host under a parked flow id = %d, want 403", st)
		}
	})
	t.Run("refusal reports are closed and sanitised", func(t *testing.T) {
		buf.Reset()
		for body, want := range map[string]int{
			`{"reason":"made-up"}`:                        http.StatusBadRequest,
			`{"reason":"udp","port":70000}`:               http.StatusBadRequest,
			`{"reason":"udp","extra":1}`:                  http.StatusBadRequest,
			`{"reason":"udp","host":"8.8.8.8","port":53}`: http.StatusNoContent,
			`{"reason":"udp","host":"a\r\nb","repeat":4}`: http.StatusNoContent,
			`{"reason":"dropped-frame","detail":"ipv6"}`:  http.StatusNoContent,
			`{"reason":"literal-ip","host":"1.1.1.1"}`:    http.StatusNoContent,
		} {
			if st := postRefusal(t, srv.URL, body); st != want {
				t.Errorf("%s = %d, want %d", body, st, want)
			}
		}
		for _, r := range waitRows(t, buf, 4) {
			if strings.ContainsAny(r.Request.Host, "\r\n") || !strings.HasPrefix(r.RuleSource, "vmnet:") {
				t.Fatalf("row %+v", r)
			}
			if r.Request.Host == "" && r.RuleSource == "vmnet:udp" && r.Repeat != 4 {
				t.Fatalf("repeat not carried: %+v", r)
			}
		}
	})
}

// TestVMFlowContainerRunUnchanged: off a VM run the relay headers mean nothing
// (an ask is an ordinary CONNECT) and the refusal route does not exist.
func TestVMFlowContainerRunUnchanged(t *testing.T) {
	upstream, dials := countingUpstream(t)
	p, buf := newRuleTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"origin.test"}}, upstream, false, "", nil)
	srv := httptest.NewServer(p)
	defer srv.Close()
	st, _, _ := stagedConnect(t, strings.TrimPrefix(srv.URL, "http://"), "origin.test:443", flowA, vmnetStageAsk)
	if st != http.StatusOK || dials.Load() != 1 {
		t.Fatalf("container CONNECT with relay headers = %d after %d dials, want an ordinary 200 tunnel", st, dials.Load())
	}
	if rs := waitRows(t, buf, 1); len(rs) != 1 || rs[0].Flow != "" {
		t.Fatalf("rows %+v: a container row never carries a flow", rs)
	}
	if st := postRefusal(t, srv.URL, `{"reason":"udp"}`); st != http.StatusNotFound {
		t.Fatalf("refusal route on a container run = %d, want 404", st)
	}
}
