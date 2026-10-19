// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// wardyn-vmnet's contract with the proxy, honoured on a VM run only (Config.VM).
// A desktop VM's only network is the relay (internal/vmnet); the relay is the
// proxy's only client, holds no token and no policy, and asks the proxy about
// every guest TCP flow in two stages so the proxy decides, holds and dials once:
//
//  1. ask, at the guest's SYN: CONNECT name:port with the flow and stage headers.
//     The whole of evaluate() runs; a deny or hold is answered and logged as
//     today, and the relay resets the SYN. An allow is parked here, answered 204,
//     and nothing is dialled or logged yet.
//  2. tunnel, once the relay has read the guest's first flight and checked its
//     TLS server name or HTTP Host: the same CONNECT with stage tunnel resumes the
//     parked decision and runs handleConnect's ordinary path (dial, allow row,
//     tunnel). A refusal the relay makes instead is reported to
//     routeVMNetRefusals, which consumes the parked decision and logs the deny,
//     so an asked flow ends in at most one row: the allow or the relay's deny
//     (the relay never rate-limits a refusal that names a flow). A flow the guest
//     abandons before its first flight expires from the table with none.
//
// A flow id the proxy does not hold (expired, or never parked because the table
// was full) is evaluated afresh: never a free pass. Container runs ignore the
// headers and the route is not found.
const (
	vmnetFlowHeader  = "Wardyn-Vmnet-Flow"
	vmnetStageHeader = "Wardyn-Vmnet-Stage"
	vmnetStageAsk    = "ask"
	vmnetStageTunnel = "tunnel"

	routeVMNetRefusals = "/wardyn/v1/vmnet/refusals"

	maxParkedFlows      = 4096
	parkedFlowTTL       = 30 * time.Second
	maxVMNetRefusalBody = 4 << 10
	maxVMNetDetail      = 253
)

// vmnetReasons are the refusals only the relay sees, each logged as rule_source
// "vmnet:<reason>" with this sentence as its cause. Closed: a report naming any
// other reason is refused.
var vmnetReasons = map[string]string{
	"literal-ip":           "the guest connected to an address the relay never gave out for a name",
	"udp":                  "the relay carries no UDP except its own DNS answers",
	"dropped-frame":        "the relay carries IPv4 TCP and its own DNS only; ICMP, IPv6 and other frames are dropped",
	"sni-mismatch":         "the TLS server name the guest sent is not the name the connection was resolved as",
	"sni-missing":          "the guest's TLS connection gave no usable server name within the relay's bound",
	"host-mismatch":        "the plain HTTP request's Host is not the name the connection was resolved as",
	"not-http":             "the guest sent something other than an HTTP request on port 80",
	"first-flight-timeout": "the guest sent nothing on a web port within the relay's wait",
}

type parkedFlow struct {
	host   string
	port   int
	target string
	log    egress.DecisionLog
	at     time.Time
}

// vmFlows holds the decisions asked flows have not yet tunnelled or refused.
type vmFlows struct {
	mu sync.Mutex
	m  map[string]parkedFlow
}

// park stores pf under id, sweeping expired entries when full. false means
// the table is full of live flows and pf was not stored.
func (f *vmFlows) park(id string, pf parkedFlow) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]parkedFlow{}
	}
	if len(f.m) >= maxParkedFlows {
		for k, v := range f.m {
			if pf.at.Sub(v.at) > parkedFlowTTL {
				delete(f.m, k)
			}
		}
		if len(f.m) >= maxParkedFlows {
			return false
		}
	}
	f.m[id] = pf
	return true
}

// claim removes and returns id's parked decision while it is still live.
func (f *vmFlows) claim(id string, now time.Time) (parkedFlow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pf, ok := f.m[id]
	delete(f.m, id)
	if !ok || now.Sub(pf.at) > parkedFlowTTL {
		return parkedFlow{}, false
	}
	return pf, true
}

// validFlowID: the relay's ids are 16 lowercase hex digits.
func validFlowID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// vmnetStage returns the relay's flow id and stage for a CONNECT, both empty
// unless this is a VM run and the id is well formed.
func (p *Proxy) vmnetStage(r *http.Request) (flow, stage string) {
	if !p.vm {
		return "", ""
	}
	flow = r.Header.Get(vmnetFlowHeader)
	if !validFlowID(flow) {
		return "", ""
	}
	switch st := r.Header.Get(vmnetStageHeader); st {
	case vmnetStageAsk, vmnetStageTunnel:
		return flow, st
	}
	return flow, ""
}

// resumeVMFlow returns the parked allow a tunnel stage resumes. resumed=false
// sends the caller to evaluate() as for any CONNECT.
// decideConnect decides a CONNECT: a relay's tunnel stage resumes the decision
// its ask parked, and anything else is evaluated (a relay row carries its flow).
func (p *Proxy) decideConnect(ctx context.Context, flow, stage, host string, port int) (egress.Decision, string, *egress.DecisionLog) {
	if d, target, log, ok := p.resumeVMFlow(ctx, flow, stage, host, port); ok {
		return d, target, log
	}
	d, target, log := p.evaluate(ctx, host, port, http.MethodConnect, "")
	if log != nil {
		log.Flow = flow
	}
	return d, target, log
}

func (p *Proxy) resumeVMFlow(ctx context.Context, flow, stage, host string, port int) (egress.Decision, string, *egress.DecisionLog, bool) {
	if stage != vmnetStageTunnel {
		return "", "", nil, false
	}
	pf, ok := p.vmFlows.claim(flow, p.now())
	if !ok || pf.host != host || pf.port != port {
		return "", "", nil, false
	}
	// The row is written under this request's spool admission, not the ask's.
	pf.log.Request.AuditReservation = spoolTicket(ctx)
	return egress.Allow, pf.target, &pf.log, true
}

// parkVMFlow answers an allowed ask: parked, nothing dialled, nothing logged.
func (p *Proxy) parkVMFlow(w http.ResponseWriter, flow, host string, port int, target string, log *egress.DecisionLog) {
	if log != nil {
		p.vmFlows.park(flow, parkedFlow{host: host, port: port, target: target, log: *log, at: p.now()})
	}
	w.WriteHeader(http.StatusNoContent)
}

// vmnetRefusal is one report from the relay. Repeat counts the refusals of the
// same kind the relay held back since its last report (its rate limit).
type vmnetRefusal struct {
	Flow   string `json:"flow,omitempty"`
	Reason string `json:"reason"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	Detail string `json:"detail,omitempty"`
	Repeat int    `json:"repeat,omitempty"`
}

// handleVMNetRefusal logs a refusal only the relay saw as an egress deny. The
// host and detail are guest-influenced, so only an IP literal or an LDH name
// reaches the row.
func (p *Proxy) handleVMNetRefusal(w http.ResponseWriter, r *http.Request) {
	var in vmnetRefusal
	dec := json.NewDecoder(io.LimitReader(r.Body, maxVMNetRefusalBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		http.Error(w, "bad vmnet refusal", http.StatusBadRequest)
		return
	}
	sentence, ok := vmnetReasons[in.Reason]
	if !ok || in.Port < 0 || in.Port > 65535 || in.Repeat < 0 {
		http.Error(w, "bad vmnet refusal", http.StatusBadRequest)
		return
	}
	req := egress.Request{RunID: p.runID, Host: vmnetName(in.Host), Port: in.Port, Time: p.now()}
	flow := ""
	if validFlowID(in.Flow) {
		flow = in.Flow
		if pf, ok := p.vmFlows.claim(flow, p.now()); ok {
			req = pf.log.Request
		}
	}
	req.AuditReservation = spoolTicket(r.Context())
	log := decisionLog(req, egress.Deny, "vmnet:"+in.Reason)
	log.Flow = flow
	log.Cause = sentence
	if d := vmnetName(in.Detail); d != "" {
		log.Cause += " (" + d + ")"
	}
	log.Repeat = in.Repeat
	p.sink.emit(log)
	w.WriteHeader(http.StatusNoContent)
}

// vmnetName keeps s only when it is an IP literal or a short LDH token.
func vmnetName(s string) string {
	if net.ParseIP(s) != nil {
		return net.ParseIP(s).String()
	}
	if s == "" || len(s) > maxVMNetDetail {
		return ""
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.', c == '_':
		default:
			return ""
		}
	}
	return s
}
