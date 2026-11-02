// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gwoffWire is a raw listener standing in for a gateway: it records each request
// exactly as it arrived on the wire and answers fixed bytes the proxy must not touch.
type gwoffWire struct {
	mu   sync.Mutex
	last string
}

const gwoffAnswer = "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: 6\r\nX-From-Gateway: yes\r\nConnection: close\r\n\r\n\x1f\x8b\x00\x01\x02\x03"

func (g *gwoffWire) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			var raw bytes.Buffer
			length, chunked := 0, false
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				raw.WriteString(line)
				if n, ok := strings.CutPrefix(line, "Content-Length: "); ok {
					length, _ = strconv.Atoi(strings.TrimSpace(n))
				}
				chunked = chunked || line == "Transfer-Encoding: chunked\r\n"
				if line == "\r\n" {
					break
				}
			}
			for chunked && !strings.HasSuffix(raw.String(), "\r\n0\r\n\r\n") {
				b, err := br.ReadByte()
				if err != nil {
					return
				}
				raw.WriteByte(b)
			}
			if _, err := io.CopyN(&raw, br, int64(length)); err != nil {
				return
			}
			g.mu.Lock()
			g.last = raw.String()
			g.mu.Unlock()
			_, _ = io.WriteString(c, gwoffAnswer)
		}()
	}
}

func (g *gwoffWire) lastRequest() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

// TestGatewayIdentityOff_WireBytesUnchanged pins what a run with no gateway route
// sends and relays, byte for byte, on the brokered, MITM and plain lanes: the
// sandbox's own X-Wardyn-* headers pass as any other header does, Accept-Encoding
// is kept, and a compressed answer comes back compressed. It names nothing of the
// route table, so it also runs unchanged against a tree from before it, and its
// goldens are that tree's bytes.
func TestGatewayIdentityOff_WireBytesUnchanged(t *testing.T) {
	const gwHost, plainHost = "gw.corp.test", "plain.corp.test"
	tlsLn, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tlsLn.Close() })
	plainLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plainLn.Close() })
	secure, plain := &gwoffWire{}, &gwoffWire{}
	go secure.serve(tlsLn)
	go plain.serve(plainLn)

	p := newProxy(Options{
		RunID:  uuid.New(),
		Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{gwHost, plainHost}}),
		Sink:   &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
		Resolver: fakeResolver{m: map[string][]net.IP{
			gwHost:    ips("93.184.216.10"),
			plainHost: ips("93.184.216.20"),
		}},
		Dial: func(ctx context.Context, network, target string) (net.Conn, error) {
			addr := tlsLn.Addr().String()
			if strings.HasPrefix(target, "93.184.216.20:") {
				addr = plainLn.Addr().String()
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		Injector:        staticInj(map[string]injectedHeader{gwHost: {name: "X-Api-Key", value: "BROKERED-KEY"}}),
		LLMUpstreams:    map[string]string{anthropicHost: "https://" + gwHost + "/v1"},
		TLSClientConfig: testInsecureTLSConfig,
	})
	sandboxHeaders := func(r *http.Request) *http.Request {
		r.Header.Set("X-Wardyn-Identity", "sandbox-forged")
		r.Header.Set("X-Wardyn-Call-Id", "sandbox-call")
		r.Header.Set("Accept-Encoding", "gzip")
		r.Header.Set("Content-Type", "application/json")
		return r
	}
	const body = `{"hi":1}`
	for _, tc := range []struct {
		name string
		wire *gwoffWire
		send func(*httptest.ResponseRecorder)
		want string
	}{
		{
			name: "brokered",
			wire: secure,
			send: func(rec *httptest.ResponseRecorder) {
				p.handleLocalRoute(rec, sandboxHeaders(mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader(body))))
			},
			want: "POST /v1/messages HTTP/1.1\r\nHost: gw.corp.test\r\nUser-Agent: Go-http-client/1.1\r\nTransfer-Encoding: chunked\r\nAccept-Encoding: gzip\r\nContent-Type: application/json\r\nX-Api-Key: BROKERED-KEY\r\nX-Wardyn-Call-Id: sandbox-call\r\nX-Wardyn-Identity: sandbox-forged\r\n\r\n8\r\n{\"hi\":1}\r\n0\r\n\r\n",
		},
		{
			name: "mitm",
			wire: secure,
			send: func(rec *httptest.ResponseRecorder) {
				p.serveMITMRequest(rec, sandboxHeaders(httptest.NewRequest(http.MethodPost, "https://"+gwHost+"/v1/messages", strings.NewReader(body))), gwHost, 443)
			},
			want: "POST /v1/messages HTTP/1.1\r\nHost: gw.corp.test\r\nUser-Agent: Go-http-client/1.1\r\nTransfer-Encoding: chunked\r\nAccept-Encoding: gzip\r\nContent-Type: application/json\r\nX-Api-Key: BROKERED-KEY\r\nX-Wardyn-Call-Id: sandbox-call\r\nX-Wardyn-Identity: sandbox-forged\r\n\r\n8\r\n{\"hi\":1}\r\n0\r\n\r\n",
		},
		{
			name: "plain",
			wire: plain,
			send: func(rec *httptest.ResponseRecorder) {
				p.handlePlain(rec, sandboxHeaders(httptest.NewRequest(http.MethodPost, "http://"+plainHost+"/thing", strings.NewReader(body))))
			},
			want: "POST /thing HTTP/1.1\r\nHost: plain.corp.test\r\nUser-Agent: Go-http-client/1.1\r\nContent-Length: 8\r\nAccept-Encoding: gzip\r\nContent-Type: application/json\r\nX-Wardyn-Call-Id: sandbox-call\r\nX-Wardyn-Identity: sandbox-forged\r\n\r\n{\"hi\":1}",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.send(rec)
			if got := tc.wire.lastRequest(); got != tc.want {
				t.Errorf("request on the wire changed:\n got %q\nwant %q", got, tc.want)
			}
			if rec.Code != http.StatusOK || rec.Body.String() != "\x1f\x8b\x00\x01\x02\x03" ||
				rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("X-From-Gateway") != "yes" {
				t.Errorf("answer changed: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
			}
		})
	}
}
