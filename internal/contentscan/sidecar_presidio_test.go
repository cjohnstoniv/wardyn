// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// presidioSpec builds the policy block through its JSON form, so a test also covers the wire
// name of detector_sidecar_format.
func presidioSpec(t *testing.T, url, mode, minSev, onErr string) types.LLMInspectionSpec {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"mode": mode, "detector_sidecar_url": url, "detector_sidecar_format": "presidio",
		"block_min_severity": minSev, "on_scanner_error": onErr,
	})
	if err != nil {
		t.Fatal(err)
	}
	var spec types.LLMInspectionSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

// presidioServer answers every /analyze call with reply (a testdata file name or an inline
// body) and keeps the request bodies it saw.
func presidioServer(t *testing.T, reply string) (*httptest.Server, *[][]byte, *atomic.Int32) {
	t.Helper()
	if b, err := os.ReadFile("testdata/presidio/" + reply); err == nil {
		reply = string(b)
	}
	var calls atomic.Int32
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies, &calls
}

func scanPresidio(t *testing.T, spec types.LLMInspectionSpec, text string) (*Engine, Result) {
	t.Helper()
	eng, err := NewEngine(spec, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	res, _, err := eng.ScanRequest(ChannelGeneric, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return eng, res
}

// Wardyn's own detector format is what an unset detector_sidecar_format sends.
func TestSidecarWithoutAFormatKeepsWardynsRequest(t *testing.T) {
	srv, bodies, _ := presidioServer(t, `{"findings":[]}`)
	eng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectorSidecarURL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.ScanRequest(ChannelGeneric, []byte("contact jo@x.io now")); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal((*bodies)[0], &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["field_path"]; !ok || got["text"] != "contact jo@x.io now" || len(got) != 2 {
		t.Fatalf("request = %s, want Wardyn's {text, field_path}", (*bodies)[0])
	}
}

// Presidio gets exactly {text, language, score_threshold}: no entities, so the operator's own
// recognizers decide what is looked for, and no field_path, which its API does not take.
func TestPresidioRequestShape(t *testing.T) {
	srv, bodies, _ := presidioServer(t, "empty.json")
	scanPresidio(t, presidioSpec(t, srv.URL, "alert", "", ""), "contact jo@x.io now")
	if len(*bodies) != 1 {
		t.Fatalf("sidecar called %d times, want 1", len(*bodies))
	}
	var got map[string]any
	if err := json.Unmarshal((*bodies)[0], &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"text": "contact jo@x.io now", "language": "en", "score_threshold": 0.5}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("request = %v, want %v", got, want)
	}
}

func TestPresidioHitMapsToFindings(t *testing.T) {
	srv, _, _ := presidioServer(t, "hit.json")
	_, res := scanPresidio(t, presidioSpec(t, srv.URL, "alert", "", ""), "contact jo@x.io now")
	if res.Skipped || len(res.Findings) != 2 {
		t.Fatalf("result = %+v, want 2 findings and no skip", res)
	}
	email, phone := res.Findings[0], res.Findings[1]
	if email.Detector != "sidecar:EMAIL_ADDRESS" || email.Category != CategoryPII || email.Severity != SevHigh ||
		email.Offset != 8 || email.Length != 7 || email.Sample != maskedPlaceholder {
		t.Errorf("email finding = %+v", email)
	}
	if phone.Detector != "sidecar:PHONE_NUMBER" || phone.Severity != SevMedium {
		t.Errorf("phone finding = %+v, want medium below 0.85", phone)
	}
}

func TestPresidioEmptyReplyIsClean(t *testing.T) {
	srv, _, _ := presidioServer(t, "empty.json")
	_, res := scanPresidio(t, presidioSpec(t, srv.URL, "block", "medium", "block"), "contact jo@x.io now")
	if res.Skipped || len(res.Findings) != 0 {
		t.Fatalf("result = %+v, want a clean scan", res)
	}
}

// A reply that is not an array is a sidecar_error, and on_scanner_error decides, as for any
// other sidecar failure.
func TestPresidioReplyThatIsNotAnArrayIsASidecarError(t *testing.T) {
	for _, reply := range []string{"not-an-array.json", "null.json", "truncated.json", `"text"`, `7`} {
		for _, tc := range []struct {
			onErr     string
			wantBlock bool
		}{{"", false}, {"block", true}} {
			t.Run(reply+"/"+tc.onErr, func(t *testing.T) {
				srv, _, _ := presidioServer(t, reply)
				eng, res := scanPresidio(t, presidioSpec(t, srv.URL, "block", "medium", tc.onErr), "contact jo@x.io now")
				if !res.Skipped || res.SkipReason != "sidecar_error" || len(res.Findings) != 0 {
					t.Fatalf("result = %+v, want skipped sidecar_error", res)
				}
				if got := eng.ShouldBlock(res); got != tc.wantBlock {
					t.Fatalf("ShouldBlock = %v, want %v", got, tc.wantBlock)
				}
			})
		}
	}
}

func TestPresidioNon200IsASidecarError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"x"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	_, res := scanPresidio(t, presidioSpec(t, srv.URL, "alert", "", ""), "contact jo@x.io now")
	if !res.Skipped || res.SkipReason != "sidecar_error" {
		t.Fatalf("result = %+v, want skipped sidecar_error", res)
	}
}

// Presidio counts code points; the finding carries bytes. The text has a two-byte letter, two
// three-byte letters and a four-byte emoji ahead of the match.
func TestPresidioOffsetsAreCodePointsConvertedToBytes(t *testing.T) {
	prefix := "héllo 日本 😀 mail "
	match := "jo@x.io"
	text := prefix + match + " end"
	start := len([]rune(prefix))
	reply := fmt.Sprintf(`[{"entity_type":"EMAIL_ADDRESS","start":%d,"end":%d,"score":1.0}]`, start, start+len([]rune(match)))
	srv, _, _ := presidioServer(t, reply)
	_, res := scanPresidio(t, presidioSpec(t, srv.URL, "alert", "", ""), text)
	if len(res.Findings) != 1 {
		t.Fatalf("findings = %+v, want 1", res.Findings)
	}
	f := res.Findings[0]
	if f.Offset != len(prefix) || f.Length != len(match) || text[f.Offset:f.Offset+f.Length] != match {
		t.Fatalf("offset/length = %d/%d, want %d/%d (the match is %q)", f.Offset, f.Length, len(prefix), len(match), match)
	}
}

// A span's reply may point past the text; the finding stays (a block is the safe side) and
// clamps to the text, and nothing panics.
func TestPresidioOutOfRangeOffsetsClampToTheText(t *testing.T) {
	text := "héllo wörld"
	for _, reply := range []string{
		`[{"entity_type":"PERSON","start":6,"end":999,"score":0.9}]`,
		`[{"entity_type":"PERSON","start":-5,"end":3,"score":0.9}]`,
		`[{"entity_type":"PERSON","start":9,"end":2,"score":0.9}]`,
	} {
		srv, _, _ := presidioServer(t, reply)
		_, res := scanPresidio(t, presidioSpec(t, srv.URL, "alert", "", ""), text)
		if res.Skipped || len(res.Findings) != 1 {
			t.Fatalf("reply %s: result = %+v, want 1 finding", reply, res)
		}
		f := res.Findings[0]
		if f.Offset < 0 || f.Length < 0 || f.Offset+f.Length > len(text) {
			t.Errorf("reply %s: offset/length = %d/%d outside the %d-byte text", reply, f.Offset, f.Length, len(text))
		}
	}
}

func TestPresidioSkipsAnEmptySpan(t *testing.T) {
	srv, _, calls := presidioServer(t, "empty.json")
	scanPresidio(t, presidioSpec(t, srv.URL, "alert", "", ""), `["", "  ", "real text here"]`)
	if got := calls.Load(); got != 1 {
		t.Fatalf("sidecar called %d times, want 1 (only the non-blank span)", got)
	}
}

func TestPresidioSeverity(t *testing.T) {
	low := []string{"PERSON", "LOCATION", "NRP", "ORGANIZATION", "DATE_TIME", "URL", "UUID", "MAC_ADDRESS", "IP_ADDRESS", "US_DRIVER_LICENSE"}
	for _, ent := range low {
		for _, score := range []float64{0.5, 0.85, 1.0} {
			if got := presidioSeverityOf(t, ent, score); got != SevLow {
				t.Errorf("%s at %.2f = %s, want low", ent, score, got)
			}
		}
	}
	for _, ent := range []string{"EMAIL_ADDRESS", "CREDIT_CARD", "US_SSN", "IBAN_CODE", "CRYPTO", "SOMETHING_CUSTOM"} {
		for score, want := range map[float64]Severity{0.5: SevMedium, 0.84: SevMedium, 0.85: SevHigh, 1.0: SevHigh} {
			if got := presidioSeverityOf(t, ent, score); got != want {
				t.Errorf("%s at %.2f = %s, want %s", ent, score, got, want)
			}
		}
	}
}

func presidioSeverityOf(t *testing.T, entity string, score float64) Severity {
	t.Helper()
	srv, _, _ := presidioServer(t, fmt.Sprintf(`[{"entity_type":%q,"start":0,"end":4,"score":%v}]`, entity, score))
	_, res := scanPresidio(t, presidioSpec(t, srv.URL, "alert", "", ""), "some text")
	if len(res.Findings) != 1 {
		t.Fatalf("%s: findings = %+v", entity, res.Findings)
	}
	return res.Findings[0].Severity
}

// At medium, a prompt full of dates, names, URLs, ids, addresses and a licence-like word
// goes through, and a real hit still stops it.
func TestPresidioAtMediumLetsACodeLikePromptThrough(t *testing.T) {
	text := "2025-11-07 Alice Smith https://example.com/a/b 123e4567-e89b-12d3-a456-426614174000 10.0.0.5:8080 license AB12345 ok"
	srv, _, _ := presidioServer(t, "code-prompt.json")
	eng, res := scanPresidio(t, presidioSpec(t, srv.URL, "block", "medium", "block"), text)
	if len(res.Findings) != 6 {
		t.Fatalf("findings = %+v, want the 6 low ones", res.Findings)
	}
	if eng.ShouldBlock(res) {
		t.Fatalf("a code-like prompt was blocked at medium: %+v", res.Findings)
	}
	for _, reply := range []string{
		`[{"entity_type":"EMAIL_ADDRESS","start":0,"end":5,"score":0.9}]`,
		`[{"entity_type":"CREDIT_CARD","start":0,"end":5,"score":0.3}]`,
	} {
		srv, _, _ := presidioServer(t, reply)
		eng, res := scanPresidio(t, presidioSpec(t, srv.URL, "block", "medium", "block"), text)
		if !eng.ShouldBlock(res) {
			t.Errorf("reply %s was not blocked at medium", reply)
		}
	}
}

func TestEngineRefusesAMisconfiguredFormat(t *testing.T) {
	for name, raw := range map[string]string{
		"presidio without a url": `{"mode":"alert","detect_pii":true,"detector_sidecar_format":"presidio"}`,
		"unknown format":         `{"mode":"alert","detector_sidecar_url":"http://x:1/a","detector_sidecar_format":"custom"}`,
	} {
		var spec types.LLMInspectionSpec
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			t.Fatal(err)
		}
		if _, err := NewEngine(spec, nil); err == nil {
			t.Errorf("%s: NewEngine = nil error, want a refusal", name)
		}
	}
}

func TestSidecarFormatIsOnTheWireForm(t *testing.T) {
	if got := string(mustJSON(t, presidioSpec(t, "http://x:1/a", "alert", "", ""))); !strings.Contains(got, `"detector_sidecar_format":"presidio"`) {
		t.Errorf("spec round trip = %s, want detector_sidecar_format kept", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
