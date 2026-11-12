// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runneractivity"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// relayMITMProxy is a VM run's proxy that terminates the model host's CONNECT and inspects the request inside it.
func relayMITMProxy(t *testing.T, runID uuid.UUID, sink *decisionSink, upstream *httptest.Server) (*Proxy, []byte) {
	t.Helper()
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	p := newProxy(Options{
		RunID:           runID,
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{anthropicHost}}),
		Sink:            sink,
		Scanner:         scanEngine(t, "alert", scanTestSecret),
		CA:              ca,
		MITMLLM:         true,
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(upstream)),
		TLSClientConfig: testInsecureTLSConfig,
	})
	p.vm, p.portRule = true, effectivePortRule(true, "")
	return p, certPEM
}

// sendMITMMessage opens the model host's CONNECT on the relay's listener or the agent's, and sends one
// allowed model call inside it. The inner call's allow is written through reqOf (emitLLMAllowWithFault).
func sendMITMMessage(t *testing.T, p *Proxy, relayed bool, caPEM []byte) {
	t.Helper()
	srv := httptest.NewServer(p)
	if relayed {
		srv.Close()
		srv = relayServer(p)
	}
	defer srv.Close()
	tlsConn := agentMITMConn(t, srv.URL, caPEM)
	defer tlsConn.Close()
	req, _ := http.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/messages", strings.NewReader(anthropicMessagesBody("hello")))
	req.Header.Set("Content-Type", "application/json")
	if err := req.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "llm-ok" {
		t.Fatalf("inner model call = %d %q, want 200 llm-ok", resp.StatusCode, b)
	}
}

func laneName(relayed bool) string {
	if relayed {
		return "relay lane"
	}
	return "agent listener"
}

// A guest that ignores the run's proxy settings reaches a host the proxy terminates through the relay. The
// requests inside that CONNECT are the same guest flow: their decisions are Relayed and never activity, in
// process and at both sinks. The same flow on the agent's listener is the run's own traffic and counts.
func TestRelayedMITMInnerRequest_IsNotActivity(t *testing.T) {
	for _, relayed := range []bool{true, false} {
		t.Run(laneName(relayed)+"/decision", func(t *testing.T) {
			cu := captureUpstream(t, true, "llm-ok")
			p, caPEM := relayMITMProxy(t, uuid.New(), &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 32)}, cu.srv)
			sendMITMMessage(t, p, relayed, caPEM)
			var inner *egress.DecisionLog
			for _, d := range sinkLogs(p) {
				if d.Request.Relayed != relayed {
					t.Errorf("%s %s (%s): Relayed = %v, want %v", d.Request.Method, d.Request.Path, d.RuleSource, d.Request.Relayed, relayed)
				}
				if d.RuleSource == ruleSourceLLMMITM && d.Usage == nil {
					inner = &d
				}
			}
			if inner == nil {
				t.Fatal("no scan:mitm allow for the inner model call")
			}
			if inner.Decision != egress.Allow || egress.CountsAsActivity(*inner) == relayed {
				t.Fatalf("inner call: Decision %s, CountsAsActivity %v; want an allow that counts only off the relay lane",
					inner.Decision, egress.CountsAsActivity(*inner))
			}
		})

		t.Run(laneName(relayed)+"/remote sink", func(t *testing.T) {
			rs, f := startedSink(t)
			c := accept(t, rs, f.next(), 1, 0, 16, 1<<20)
			cu := captureUpstream(t, true, "llm-ok")
			p, caPEM := relayMITMProxy(t, uuid.New(), &decisionSink{remote: rs, out: &bytes.Buffer{}}, cu.srv)
			done := make(chan struct{})
			go func() { defer close(done); sendMITMMessage(t, p, relayed, caPEM) }()
			for {
				fr := readFrame(t, c)
				if fr.T != FrameRecord {
					continue
				}
				writeFrame(t, c, SinkFrame{T: FrameAck, Gen: fr.Gen, Seq: fr.Seq, Disposition: "appended"})
				var body RecordBody
				if err := json.Unmarshal(fr.Body, &body); err != nil {
					t.Fatal(err)
				}
				if body.Activity && relayed {
					t.Fatalf("relayed record %s %s (%s) counted as activity", body.Decision.Request.Method, body.Decision.Request.Path, body.Decision.RuleSource)
				}
				if body.Decision.RuleSource == ruleSourceLLMMITM && body.Decision.Usage == nil {
					if !body.Admitted || body.Activity == relayed {
						t.Fatalf("inner call record: admitted %v, activity %v; want admitted, activity %v", body.Admitted, body.Activity, !relayed)
					}
					break
				}
			}
			<-done
		})

		t.Run(laneName(relayed)+"/local spool", func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "spool")
			id := uuid.New()
			sink, err := newLocalDecisionSink(dir, id, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer sink.close(t.Context())
			epoch := uuid.New()
			if err := runneractivity.Publish(dir, epoch); err != nil {
				t.Fatal(err)
			}
			cu := captureUpstream(t, true, "llm-ok")
			p, caPEM := relayMITMProxy(t, id, sink, cu.srv)
			sendMITMMessage(t, p, relayed, caPEM)
			m, err := runneractivity.Read(dir)
			moved := err == nil && m.Epoch == epoch
			if moved == relayed {
				t.Fatalf("activity marker moved = %v (%+v, %v); want %v", moved, m, err, !relayed)
			}
		})
	}
}
