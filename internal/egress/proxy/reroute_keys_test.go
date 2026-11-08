// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// rrRule is the rule_source of a row the inspection site writes for a rerouting key.
const rrRule = "scan:reroute-keys"

// rrKeys are the six keys, each with a value that names a model or setting the run did not
// list. Every value carries "zz", so a test can see that a row or an answer never repeats one.
var rrKeys = []struct{ key, value string }{
	{"fallbacks", `["zz-claude-opus-9"]`},
	{"models", `["zz-gpt-4"]`},
	{"context_window_fallbacks", `[{"zz-a":"zz-b"}]`},
	{"content_policy_fallbacks", `[{"zz-a":"zz-b"}]`},
	{"router_settings_override", `{"zz-k":"zz-v"}`},
	{"preset", `"@zz/preset"`},
}

// rrRoutes are the two route families on both lanes: the brokered route and a terminated tunnel.
type rrRoute struct {
	name, brokered, mitmHost, mitmPath, model string
}

var (
	rrAnthropic = rrRoute{"anthropic messages", llmAnthropicPrefix + "messages", anthropicHost, "/v1/messages", "claude-sonnet-5"}
	rrResponses = rrRoute{"openai responses", llmOpenAIPrefix + "responses", openaiHost, "/v1/responses", "gpt-5"}
	rrEveryPath = []rrRoute{
		rrAnthropic,
		{"anthropic count_tokens", llmAnthropicPrefix + "messages/count_tokens", anthropicHost, "/v1/messages/count_tokens", "claude-sonnet-5"},
		rrResponses,
		{"openai chat", llmOpenAIPrefix + "chat/completions", openaiHost, "/v1/chat/completions", "gpt-5"},
		{"openai compaction", llmOpenAIPrefix + "responses/compact", openaiHost, "/v1/responses/compact", "gpt-5"},
		{"openai completions", llmOpenAIPrefix + "completions", openaiHost, "/v1/completions", "gpt-5"},
	}
)

// rrProxy is the gateway rig with the allowed models list (nil == unset) and an inspection
// engine in mode ("" == none).
func rrProxy(t *testing.T, allowed []string, mode string) (*Proxy, *capturedUpstream, *bytes.Buffer) {
	t.Helper()
	p, up, buf := modelGateProxy(t, allowed)
	p.res = publicResolver{} // the terminated lane dials the model hosts by name
	if mode != "" {
		p.scanner = scanEngine(t, mode, scanTestSecret)
	}
	return p, up, buf
}

// rrSend sends body to the route on a lane ("brokered" or "mitm").
func rrSend(t *testing.T, p *Proxy, lane string, rt rrRoute, body string, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	switch lane {
	case "brokered":
		req := mustLocalReq(t, http.MethodPost, rt.brokered, strings.NewReader(body))
		for k, v := range hdr {
			req.Header[k] = v
		}
		p.ServeHTTP(rec, req)
	case "mitm":
		req := httptest.NewRequest(http.MethodPost, "https://"+rt.mitmHost+rt.mitmPath, strings.NewReader(body))
		for k, v := range hdr {
			req.Header[k] = v
		}
		p.serveMITMRequest(rec, req, rt.mitmHost, 443)
	default:
		t.Fatalf("lane %q", lane)
	}
	return rec
}

var rrLanes = []string{"brokered", "mitm"}

func rrBody(rt rrRoute, key, value string) string {
	return `{"model":"` + rt.model + `","` + key + `":` + value + `,"max_tokens":4}`
}

// rrRows is every decision row with the given rule source, in order.
func rrRows(t *testing.T, buf *bytes.Buffer, ruleSource string) []egress.DecisionLog {
	t.Helper()
	var out []egress.DecisionLog
	sc := bufio.NewScanner(strings.NewReader(buf.String()))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var d egress.DecisionLog
		if json.Unmarshal(sc.Bytes(), &d) == nil && d.RuleSource == ruleSource {
			out = append(out, d)
		}
	}
	return out
}

func rrAllowed() []string { return []string{"claude-sonnet-5", "gpt-5"} }

// With the allowed models list set, a body carrying any of the six keys is refused on every
// route that carries a model, on both lanes, whatever the inspection mode says: the keys let
// the sandbox pick a model the list never named. The row names the key and never its value.
func TestRerouteKeys_GateRefusesEachKey(t *testing.T) {
	for _, mode := range []string{"", "alert", "block"} {
		for _, lane := range rrLanes {
			for _, rt := range []rrRoute{rrAnthropic, rrResponses} {
				for _, k := range rrKeys {
					t.Run(mode+"/"+lane+"/"+rt.name+"/"+k.key, func(t *testing.T) {
						p, up, buf := rrProxy(t, rrAllowed(), mode)
						rec := rrSend(t, p, lane, rt, rrBody(rt, k.key, k.value), nil)
						if rec.Code != http.StatusForbidden || up.reached {
							t.Fatalf("status = %d, reached = %v; want a 403 and nothing sent (%s)", rec.Code, up.reached, rec.Body)
						}
						d := lastDecision(t, buf)
						if d.Decision != egress.Deny || d.RuleSource != ruleSourceAllowedModels || !strings.Contains(d.Cause, k.key) {
							t.Fatalf("row = %+v; want an allowed-models deny whose cause names %q", d, k.key)
						}
						if strings.Contains(d.Cause, "zz") || strings.Contains(rec.Body.String(), "zz") {
							t.Fatalf("a value reached the row or the answer: cause %q answer %q", d.Cause, rec.Body)
						}
					})
				}
			}
		}
	}
}

// Every route that carries a model is covered, and the key matches the way the model key does:
// without regard to case. A key nested deeper, or one that only starts like a listed key, is
// not a rerouting key.
func TestRerouteKeys_GateRoutesAndSpellings(t *testing.T) {
	for _, lane := range rrLanes {
		for _, rt := range rrEveryPath {
			t.Run(lane+"/"+rt.name, func(t *testing.T) {
				p, up, _ := rrProxy(t, rrAllowed(), "")
				if rec := rrSend(t, p, lane, rt, rrBody(rt, "fallbacks", `["zz"]`), nil); rec.Code != http.StatusForbidden || up.reached {
					t.Fatalf("fallbacks: status = %d, reached = %v; want a 403", rec.Code, up.reached)
				}
			})
		}
	}
	for _, tc := range []struct {
		name, body string
		refused    bool
	}{
		{"upper case key", `{"model":"claude-sonnet-5","Fallbacks":["zz"]}`, true},
		{"escaped key", `{"model":"claude-sonnet-5","fallbacks":["zz"]}`, true},
		{"null value", `{"model":"claude-sonnet-5","preset":null}`, true},
		{"empty list", `{"model":"claude-sonnet-5","models":[]}`, true},
		{"nested key", `{"model":"claude-sonnet-5","metadata":{"fallbacks":["zz"]}}`, false},
		{"longer key", `{"model":"claude-sonnet-5","fallbacks_note":"zz"}`, false},
		{"no key", `{"model":"claude-sonnet-5","max_tokens":4}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, up, _ := rrProxy(t, rrAllowed(), "")
			rec := rrSend(t, p, "brokered", rrAnthropic, tc.body, nil)
			if tc.refused == (rec.Code == http.StatusOK) || tc.refused == up.reached {
				t.Fatalf("status = %d, reached = %v, want refused = %v (%s)", rec.Code, up.reached, tc.refused, rec.Body)
			}
		})
	}
}

// Bodies the gate cannot read stay refused with a key in them: oversize, encoded, repeated key.
func TestRerouteKeys_GateUnparsableBodiesRefused(t *testing.T) {
	old := maxLLMScanBody
	maxLLMScanBody = 128
	t.Cleanup(func() { maxLLMScanBody = old })
	big := `{"model":"claude-sonnet-5","fallbacks":["` + strings.Repeat("a", 200) + `"]}`
	gz := http.Header{"Content-Encoding": {"gzip"}}
	for _, tc := range []struct {
		name, body string
		hdr        http.Header
	}{
		{"oversize", big, nil},
		{"gzip", `{"model":"claude-sonnet-5","fallbacks":["zz"]}`, gz},
		{"repeated key", `{"model":"claude-sonnet-5","fallbacks":["a"],"Fallbacks":["b"]}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, up, _ := rrProxy(t, rrAllowed(), "")
			if rec := rrSend(t, p, "brokered", rrAnthropic, tc.body, tc.hdr); rec.Code < 400 || up.reached {
				t.Fatalf("status = %d, reached = %v; want a refusal", rec.Code, up.reached)
			}
		})
	}
}

// With no allowed models list and no inspection, the keys are not looked at: the request passes.
func TestRerouteKeys_NoGateNoCheck(t *testing.T) {
	for _, lane := range rrLanes {
		p, up, _ := rrProxy(t, nil, "")
		body := rrBody(rrAnthropic, "fallbacks", `["zz"]`)
		if rec := rrSend(t, p, lane, rrAnthropic, body, nil); rec.Code != http.StatusOK || up.body != body {
			t.Fatalf("%s: status = %d, upstream body %q; want it forwarded unchanged", lane, rec.Code, up.body)
		}
	}
}

// In block mode, inspection refuses a body carrying any of the six keys before the scan runs, on
// every route family and both lanes: the Responses route is one the scan cannot read, and a
// key there is refused all the same. The row is a deny that names the key and never its value.
func TestRerouteKeys_InspectionBlocksEachKey(t *testing.T) {
	for _, lane := range rrLanes {
		for _, rt := range []rrRoute{rrAnthropic, rrResponses} {
			for _, k := range rrKeys {
				t.Run(lane+"/"+rt.name+"/"+k.key, func(t *testing.T) {
					p, up, buf := rrProxy(t, nil, "block")
					rec := rrSend(t, p, lane, rt, rrBody(rt, k.key, k.value), nil)
					if rec.Code != http.StatusForbidden || up.reached {
						t.Fatalf("status = %d, reached = %v; want a 403 and nothing sent (%s)", rec.Code, up.reached, rec.Body)
					}
					rows := rrRows(t, buf, rrRule)
					if len(rows) != 1 || rows[0].Decision != egress.Deny || !strings.Contains(rows[0].Cause, k.key) {
						t.Fatalf("rows = %+v; want one %s deny naming %q", rows, rrRule, k.key)
					}
					if strings.Contains(rows[0].Cause, "zz") || strings.Contains(rec.Body.String(), "zz") {
						t.Fatalf("a value reached the row or the answer: cause %q answer %q", rows[0].Cause, rec.Body)
					}
				})
			}
		}
	}
}

// A body naming several keys is refused once, and the row names each.
func TestRerouteKeys_RowNamesEveryKeyFound(t *testing.T) {
	p, _, buf := rrProxy(t, nil, "block")
	body := `{"model":"gpt-5","preset":"@zz/p","input":"x","models":["zz"]}`
	if rec := rrSend(t, p, "brokered", rrResponses, body, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	rows := rrRows(t, buf, rrRule)
	if len(rows) != 1 || !strings.Contains(rows[0].Cause, "preset") || !strings.Contains(rows[0].Cause, "models") {
		t.Fatalf("rows = %+v; want one row naming preset and models", rows)
	}
}

// Every route that carries a model is covered at the inspection site too.
func TestRerouteKeys_InspectionCoversEveryRoute(t *testing.T) {
	for _, lane := range rrLanes {
		for _, rt := range rrEveryPath {
			t.Run(lane+"/"+rt.name, func(t *testing.T) {
				p, up, _ := rrProxy(t, nil, "block")
				if rec := rrSend(t, p, lane, rt, rrBody(rt, "fallbacks", `["zz"]`), nil); rec.Code != http.StatusForbidden || up.reached {
					t.Fatalf("status = %d, reached = %v; want a 403", rec.Code, up.reached)
				}
			})
		}
	}
}

// In alert mode the request goes on, byte for byte, and leaves one allow row naming the keys.
func TestRerouteKeys_AlertForwardsAndLeavesARow(t *testing.T) {
	for _, lane := range rrLanes {
		for _, rt := range []rrRoute{rrAnthropic, rrResponses} {
			for _, k := range rrKeys {
				t.Run(lane+"/"+rt.name+"/"+k.key, func(t *testing.T) {
					p, up, buf := rrProxy(t, nil, "alert")
					body := rrBody(rt, k.key, k.value)
					if rec := rrSend(t, p, lane, rt, body, nil); rec.Code != http.StatusOK || up.body != body {
						t.Fatalf("status = %d, upstream body %q; want it forwarded unchanged", rec.Code, up.body)
					}
					rows := rrRows(t, buf, rrRule)
					if len(rows) != 1 || rows[0].Decision != egress.Allow || !strings.Contains(rows[0].Cause, k.key) || strings.Contains(rows[0].Cause, "zz") {
						t.Fatalf("rows = %+v; want one allow row naming %q and no value", rows, k.key)
					}
				})
			}
		}
	}
}

// A body with none of the keys leaves no row of this kind and is not touched.
func TestRerouteKeys_CleanBodyLeavesNoRow(t *testing.T) {
	for _, mode := range []string{"alert", "block"} {
		p, up, buf := rrProxy(t, nil, mode)
		body := `{"model":"claude-sonnet-5","max_tokens":4,"metadata":{"fallbacks":["nested is not a key"]}}`
		if rec := rrSend(t, p, "brokered", rrAnthropic, body, nil); rec.Code != http.StatusOK || up.body != body {
			t.Fatalf("%s: status = %d, upstream body %q", mode, rec.Code, up.body)
		}
		if rows := rrRows(t, buf, rrRule); len(rows) != 0 {
			t.Fatalf("%s: rows = %+v, want none", mode, rows)
		}
	}
}

// A body the check cannot read: block refuses it, alert forwards it unchanged and leaves a row
// with reason unparsable.
func TestRerouteKeys_UnparsableBodies(t *testing.T) {
	old := maxLLMScanBody
	maxLLMScanBody = 128
	t.Cleanup(func() { maxLLMScanBody = old })
	gz := http.Header{"Content-Encoding": {"gzip"}}
	for _, tc := range []struct {
		name, body string
		hdr        http.Header
	}{
		{"oversize", `{"model":"claude-sonnet-5","note":"` + strings.Repeat("a", 200) + `"}`, nil},
		{"encoded", `{"model":"claude-sonnet-5","max_tokens":4}`, gz},
		{"not an object", `["claude-sonnet-5"]`, nil},
		{"repeated key", `{"model":"claude-sonnet-5","fallbacks":["a"],"fallbacks":["b"]}`, nil},
		{"repeated by case", `{"model":"claude-sonnet-5","preset":"a","Preset":"b"}`, nil},
		{"cut off", `{"model":"claude-sonnet-5","fallbacks":[`, nil},
	} {
		for _, lane := range rrLanes {
			t.Run("block/"+lane+"/"+tc.name, func(t *testing.T) {
				p, up, buf := rrProxy(t, nil, "block")
				rec := rrSend(t, p, lane, rrAnthropic, tc.body, tc.hdr)
				if rec.Code != http.StatusForbidden || up.reached {
					t.Fatalf("status = %d, reached = %v; want a 403 and nothing sent", rec.Code, up.reached)
				}
				rows := rrRows(t, buf, rrRule)
				if len(rows) != 1 || rows[0].Decision != egress.Deny || !strings.Contains(rows[0].Cause, "unparsable") {
					t.Fatalf("rows = %+v; want one deny with reason unparsable", rows)
				}
			})
			t.Run("alert/"+lane+"/"+tc.name, func(t *testing.T) {
				p, up, buf := rrProxy(t, nil, "alert")
				rec := rrSend(t, p, lane, rrAnthropic, tc.body, tc.hdr)
				if rec.Code != http.StatusOK || up.body != tc.body {
					t.Fatalf("status = %d, upstream body %q; want it forwarded unchanged", rec.Code, up.body)
				}
				rows := rrRows(t, buf, rrRule)
				if len(rows) != 1 || rows[0].Decision != egress.Allow || !strings.Contains(rows[0].Cause, "unparsable") {
					t.Fatalf("rows = %+v; want one allow row with reason unparsable", rows)
				}
			})
		}
	}
}

// A request that is not a model call is not read for the keys: a body that would be unparsable
// on a model route passes on a route that carries no model, and a read-only method is quiet.
func TestRerouteKeys_OnlyModelRoutesAreRead(t *testing.T) {
	p, _, buf := rrProxy(t, nil, "block")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, llmAnthropicPrefix+"models", nil))
	if rows := rrRows(t, buf, rrRule); len(rows) != 0 {
		t.Fatalf("a model listing left rows %+v", rows)
	}
}

// With the gateway checking prompts itself (inspection: gateway) the proxy leaves the keys to
// it: a body that block mode would refuse is sent, and no row of this kind is written.
func TestRerouteKeys_GatewayInspectionLeavesTheKeysToTheGateway(t *testing.T) {
	for _, lane := range rrLanes {
		t.Run(lane, func(t *testing.T) {
			g := newInspectRig(t, deferredRoutes(), inspSpec(true))
			body := `{"model":"m","max_tokens":8,"fallbacks":["zz"],"messages":[{"role":"user","content":"hi"}]}`
			rec := httptest.NewRecorder()
			switch lane {
			case "brokered":
				g.p.handleLocalRoute(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader(body)))
			case "mitm":
				g.p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://"+gwidHost+"/v1/messages", strings.NewReader(body)), gwidHost, 443)
			}
			if rec.Code != http.StatusOK || g.gw.count() != 1 {
				t.Fatalf("status = %d, gateway saw %d; want the request sent", rec.Code, g.gw.count())
			}
			if rows := rrRows(t, g.buf, rrRule); len(rows) != 0 {
				t.Fatalf("rows = %+v, want none: the gateway reads these keys itself", rows)
			}
			// The same body on a route whose switch is off is refused, so the test is not vacuous.
			off := newInspectRig(t, gwidRoutes(), inspSpec(true))
			rec = httptest.NewRecorder()
			off.p.handleLocalRoute(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader(body)))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("switch off: status = %d, want 403", rec.Code)
			}
		})
	}
}

// One body at the scan cap runs the key check, the scan and the metering, with the retained
// budget sized to that one body: the check keeps the body charged (and charges it once) for the
// scan and the meter, which read the bytes the check already holds.
func TestRerouteKeys_OneNearCapBodyThroughCheckScanAndMeter(t *testing.T) {
	const capBytes = 4096
	old := maxLLMScanBody
	maxLLMScanBody = capBytes
	t.Cleanup(func() { maxLLMScanBody = old })

	head := `{"model":"claude-sonnet-5","max_tokens":4,"fallbacks":["zz"],"messages":[{"role":"user","content":"` + scanTestSecret + ` `
	tail := `"}]}`
	body := head + strings.Repeat("a", capBytes-len(head)-len(tail)) + tail
	if len(body) != capBytes {
		t.Fatalf("test body is %d bytes, want %d", len(body), capBytes)
	}
	for _, lane := range rrLanes {
		t.Run(lane, func(t *testing.T) {
			up := captureUpstream(t, true, `{"type":"message","usage":{"input_tokens":3,"output_tokens":2}}`)
			p, _, buf := rrProxy(t, nil, "alert")
			p.dial = redirectDial(upstreamAddr(up.srv))
			shrinkRetainedBudget(t, p, capBytes)
			if rec := rrSend(t, p, lane, rrAnthropic, body, nil); rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
			}
			if up.body != body {
				t.Fatalf("the upstream got %d bytes, want the %d the sandbox sent, unchanged", len(up.body), len(body))
			}
			if rows := rrRows(t, buf, rrRule); len(rows) != 1 || !strings.Contains(rows[0].Cause, "fallbacks") {
				t.Fatalf("key rows = %+v, want one naming fallbacks", rows)
			}
			scanned := false
			for _, d := range rrRows(t, buf, ruleSourceLLM) {
				scanned = scanned || (d.Scan != nil && len(d.Scan.Findings) > 0)
			}
			for _, d := range rrRows(t, buf, ruleSourceLLMMITM) {
				scanned = scanned || (d.Scan != nil && len(d.Scan.Findings) > 0)
			}
			if !scanned {
				t.Fatalf("the scan found nothing in the body; its secret must still be seen after the key check:\n%s", buf)
			}
			if u := usageRows(t, buf); len(u) != 1 || u[0].Usage == nil || u[0].Usage.Model != "claude-sonnet-5" {
				t.Fatalf("usage rows = %+v, want one metered with the request's model", u)
			}
			if !retainedBudgetEmpty(p) {
				t.Fatal("bytes still charged after the request returned")
			}
		})
	}
}

// A body of no declared length, longer than the scan cap, goes on whole in alert mode: the check
// reads the cap, writes its row, and the rest follows, with nothing charged after the request.
func TestRerouteKeys_UnsizedOversizeBodyForwardedWhole(t *testing.T) {
	old := maxLLMScanBody
	maxLLMScanBody = 128
	t.Cleanup(func() { maxLLMScanBody = old })
	body := `{"model":"claude-sonnet-5","note":"` + strings.Repeat("a", 5000) + `"}`
	p, up, buf := rrProxy(t, nil, "alert")
	req := mustLocalReq(t, http.MethodPost, rrAnthropic.brokered, strings.NewReader(body))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || up.body != body {
		t.Fatalf("status = %d, upstream got %d bytes, want the %d sent", rec.Code, len(up.body), len(body))
	}
	if rows := rrRows(t, buf, rrRule); len(rows) != 1 || !strings.Contains(rows[0].Cause, "unparsable") {
		t.Fatalf("rows = %+v, want one unparsable row", rows)
	}
	if !retainedBudgetEmpty(p) {
		t.Fatal("bytes still charged after the request returned")
	}
}
