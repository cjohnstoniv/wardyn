// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	testSecret = "sk-ant-supersecret-DEADBEEF-0123456789" // >= MinLen
	otherText  = "this is an ordinary prompt with no secrets in it"
)

func newTestEngine(t *testing.T, mode string, secrets ...string) *Engine {
	t.Helper()
	corpus := make([][]byte, 0, len(secrets))
	for _, s := range secrets {
		corpus = append(corpus, []byte(s))
	}
	eng, err := NewEngine(types.LLMInspectionSpec{
		Mode:          mode,
		DetectSecrets: true,
	}, corpus)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return eng
}

func anthropicBody(t *testing.T, system string, messages ...string) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`{"model":"claude","max_tokens":10`)
	if system != "" {
		sb.WriteString(`,"system":` + jsonString(system))
	}
	sb.WriteString(`,"messages":[`)
	for i, m := range messages {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"role":"user","content":` + jsonString(m) + `}`)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func TestNewEngine_OffAndEmptyCorpusDisable(t *testing.T) {
	if eng, _ := NewEngine(types.LLMInspectionSpec{Mode: "off", DetectSecrets: true}, [][]byte{[]byte(testSecret)}); eng != nil {
		t.Fatal("mode=off must return a nil engine")
	}
	if eng, _ := NewEngine(types.LLMInspectionSpec{Mode: "", DetectSecrets: true}, [][]byte{[]byte(testSecret)}); eng != nil {
		t.Fatal("empty mode must return a nil engine")
	}
	if eng, _ := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true}, nil); eng != nil {
		t.Fatal("empty corpus must return a nil (disabled) engine")
	}
	if _, err := NewEngine(types.LLMInspectionSpec{Mode: "bogus", DetectSecrets: true}, [][]byte{[]byte(testSecret)}); err == nil {
		t.Fatal("invalid mode must error")
	}
}

func TestScan_DetectsKnownSecretInLastMessage(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)
	body := anthropicBody(t, "", "here is the key: "+testSecret)
	res, out, err := eng.ScanRequest(ChannelAnthropicMessages, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if string(out) != string(body) {
		t.Fatal("v1 must not rewrite the body")
	}
	if len(res.Findings) == 0 {
		t.Fatal("expected a finding")
	}
	f := res.Findings[0]
	if f.Detector != "known-secret" || f.Category != CategorySecret || f.Severity != SevCritical {
		t.Fatalf("unexpected finding: %+v", f)
	}
	if !strings.HasPrefix(f.FieldPath, "messages[0].content") {
		t.Fatalf("unexpected field path %q", f.FieldPath)
	}
}

func TestScan_CleanPromptNoFindings(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "you are helpful", otherText))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected no findings, got %+v", res.Findings)
	}
}

func TestScan_OnlyLastMessageScanned(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)
	// secret in an EARLIER message must not be found (it was scanned when it was
	// newest); only the last message is scanned this turn.
	body := anthropicBody(t, "", "old turn with "+testSecret, "fresh clean turn")
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("earlier-message secret must not be re-scanned, got %+v", res.Findings)
	}
}

func TestScan_SystemPromptScanned(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "system has "+testSecret, "clean"))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) == 0 || res.Findings[0].FieldPath != "system" {
		t.Fatalf("expected a system finding, got %+v", res.Findings)
	}
}

func TestScan_ToolUseInputAndToolResult(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)
	// tool_use input (nested string leaf) + tool_result content as the last msg.
	body := []byte(`{"model":"claude","max_tokens":10,"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","name":"db","input":{"conn":{"pw":"` + testSecret + `"}}}]}` +
		`]}`)
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) == 0 {
		t.Fatal("expected a finding inside tool_use.input")
	}
	if !strings.Contains(res.Findings[0].FieldPath, ".input.conn.pw") {
		t.Fatalf("unexpected field path %q", res.Findings[0].FieldPath)
	}
}

func TestScan_NeverEmitsRawSecret(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)
	res, _, _ := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "leak "+testSecret))
	if len(res.Findings) == 0 {
		t.Fatal("expected a finding")
	}
	for _, f := range res.Findings {
		if f.Sample != maskedPlaceholder {
			t.Fatalf("sample must be the masked placeholder, got %q", f.Sample)
		}
		if strings.Contains(f.Detector+f.FieldPath+f.Sample, testSecret) {
			t.Fatal("a finding leaked the raw secret")
		}
	}
}

func TestScan_NormalizedVariants(t *testing.T) {
	// A secret with chars that genuinely percent-/base64-/hex-encode away from
	// the raw bytes, so each decode path is actually exercised (not satisfied by
	// the raw exact-match).
	const enc = "p@ss word DEADBEEF 0123456789"
	eng := newTestEngine(t, "alert", enc)
	cases := map[string]string{
		"base64": base64.StdEncoding.EncodeToString([]byte(enc)),
		"hex":    hex.EncodeToString([]byte(enc)),
		"url":    url.PathEscape(enc),
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			if encoded == enc {
				t.Fatalf("%s encoding is a no-op; test would not exercise decode", name)
			}
			res, _, err := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", encoded))
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(res.Findings) == 0 {
				t.Fatalf("%s-encoded secret should be caught by normalization", name)
			}
			if !strings.HasPrefix(res.Findings[0].Detector, "known-secret") {
				t.Fatalf("unexpected detector %q", res.Findings[0].Detector)
			}
		})
	}
}

// TestDedupeFindings_DistinctVariantsSameOffset asserts two DISTINCT decoded-
// variant secret hits at the same field/offset (both Offset == -1, the decoded-
// space sentinel) are counted separately, not collapsed into one. Undercounting
// here would hide that multiple secrets leaked through the same encoding. True
// exact duplicates (identical detector+field+offset+length) still collapse.
func TestDedupeFindings_DistinctVariantsSameOffset(t *testing.T) {
	in := []Finding{
		// Two DIFFERENT secrets matched inside the SAME base64-decoded variant:
		// same detector chain, same field, both Offset -1, but different lengths.
		{Detector: "known-secret:base64", FieldPath: "$body", Offset: -1, Length: 10, matchID: 1},
		{Detector: "known-secret:base64", FieldPath: "$body", Offset: -1, Length: 12, matchID: 2},
		// An exact duplicate of the first — must collapse.
		{Detector: "known-secret:base64", FieldPath: "$body", Offset: -1, Length: 10, matchID: 1},
	}
	out := dedupeFindings(in)
	if len(out) != 2 {
		t.Fatalf("want 2 distinct findings, got %d: %+v", len(out), out)
	}
}

// Two DISTINCT corpus secrets of IDENTICAL length in the SAME decoded variant
// must NOT collapse — the corpus-index discriminator (matchID) separates them
// even though (detector, field, offset, length) are identical. Same-provider
// API keys are the common identical-length case, so this is the load-bearing
// undercount guard.
func TestDedupeFindings_DistinctSecretsIdenticalLength(t *testing.T) {
	in := []Finding{
		{Detector: "known-secret:base64", FieldPath: "$body", Offset: -1, Length: 40, matchID: 1},
		{Detector: "known-secret:base64", FieldPath: "$body", Offset: -1, Length: 40, matchID: 2},
		// A true re-emission of the first corpus secret (same matchID) collapses.
		{Detector: "known-secret:base64", FieldPath: "$body", Offset: -1, Length: 40, matchID: 1},
	}
	out := dedupeFindings(in)
	if len(out) != 2 {
		t.Fatalf("two distinct same-length corpus secrets must stay 2 findings, got %d: %+v", len(out), out)
	}
}

func TestScan_SecretAsKeyMaskedFromFieldPath(t *testing.T) {
	// An agent places the secret as BOTH a tool_use.input key and a nested value.
	// The value triggers a finding; the field path (built from the key) must be
	// masked so the (SIEM-fanned) audit record stays content-free.
	eng := newTestEngine(t, "alert", testSecret)
	body := []byte(`{"model":"c","max_tokens":1,"messages":[{"role":"assistant","content":` +
		`[{"type":"tool_use","name":"x","input":{"` + testSecret + `":{"k":"` + testSecret + `"}}}]}]}`)
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) == 0 {
		t.Fatal("expected a finding")
	}
	for _, f := range res.Findings {
		if strings.Contains(f.FieldPath, testSecret) {
			t.Fatalf("field path leaked the raw secret: %q", f.FieldPath)
		}
		if !strings.Contains(f.FieldPath, maskedPlaceholder) {
			t.Fatalf("secret-bearing key should be masked in path, got %q", f.FieldPath)
		}
	}
}

func TestScan_RegexFormatKeyMaskedFromFieldPath(t *testing.T) {
	// A regex-FORMAT secret (ghp_…) used as a JSON key, with a corpus secret as the
	// value: the known-secret finding's path must NOT leak the ghp_ key (the path
	// is masked by BOTH the corpus masker and the regex catalog).
	eng := newTestEngine(t, "alert", testSecret)
	ghpKey := "ghp_" + strings.Repeat("a", 36)
	body := []byte(`{"model":"c","max_tokens":1,"messages":[{"role":"assistant","content":` +
		`[{"type":"tool_use","name":"x","input":{"` + ghpKey + `":"` + testSecret + `"}}]}]}`)
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) == 0 {
		t.Fatal("expected a known-secret finding")
	}
	for _, f := range res.Findings {
		if strings.Contains(f.FieldPath, "ghp_") {
			t.Fatalf("field path leaked the regex-format key: %q", f.FieldPath)
		}
	}
}

func TestScan_CorpusSecretAsKeyMaskedForNonSecretDetector(t *testing.T) {
	// FIX #3: a corpus secret used as an agent-controlled JSON KEY must be masked
	// from a Finding's FieldPath even when the finding is produced by a NON-corpus
	// detector (here pii) with the known-secret detector DISABLED. Otherwise the
	// raw corpus secret rides verbatim into the append-only, SIEM-fanned audit via
	// FieldPath — breaking the package's content-free invariant. The central
	// chokepoint (Engine.sanitizeFieldPath) covers every detector's findings.
	eng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectPII: true},
		[][]byte{[]byte(testSecret)})
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// testSecret is the JSON KEY; an email is the leaf value that triggers the pii
	// hit, so the resulting FieldPath is built from the secret-bearing key.
	body := []byte(`{"model":"c","max_tokens":1,"messages":[{"role":"assistant","content":` +
		`[{"type":"tool_use","name":"x","input":{"` + testSecret + `":"alice@example.com"}}]}]}`)
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) == 0 {
		t.Fatal("expected a pii finding on the email value")
	}
	for _, f := range res.Findings {
		if f.Category != CategoryPII {
			t.Fatalf("expected only pii findings (known-secret disabled), got %+v", f)
		}
		if strings.Contains(f.FieldPath, testSecret) {
			t.Fatalf("field path leaked the raw corpus secret via a non-secret detector: %q", f.FieldPath)
		}
		if !strings.Contains(f.FieldPath, maskedPlaceholder) {
			t.Fatalf("corpus-secret key must be masked in path, got %q", f.FieldPath)
		}
	}
}

func TestNewEngine_FailClosedOnInvalidDetectors(t *testing.T) {
	corpus := [][]byte{[]byte(testSecret)}
	// Mode set with NO detector must error (cannot silently disable).
	if _, err := NewEngine(types.LLMInspectionSpec{Mode: "block"}, corpus); err == nil {
		t.Fatal("mode without a detector must error")
	}
	// Pattern/entropy detectors need no corpus and build fine.
	if eng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectSecretPatterns: true}, nil); err != nil || eng == nil {
		t.Fatalf("detect_secret_patterns should build with no corpus: eng=%v err=%v", eng, err)
	}
	if eng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectEntropy: true}, nil); err != nil || eng == nil {
		t.Fatalf("detect_entropy should build with no corpus: eng=%v err=%v", eng, err)
	}
}

func TestRegexSecretCatalog(t *testing.T) {
	eng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectSecretPatterns: true}, nil)
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	for name, secret := range map[string]string{
		"aws":    "here is AKIAIOSFODNN7EXAMPLE ok",
		"github": "token ghp_" + strings.Repeat("a", 36),
		"google": "key AIza" + strings.Repeat("b", 35),
		"stripe": "sk_live_" + strings.Repeat("c", 24),
		"pem":    "-----BEGIN RSA PRIVATE KEY-----",
	} {
		t.Run(name, func(t *testing.T) {
			res, _, err := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", secret))
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(res.Findings) == 0 {
				t.Fatalf("%s pattern not detected", name)
			}
			if !strings.HasPrefix(res.Findings[0].Detector, "regex:") || res.Findings[0].Sample != maskedPlaceholder {
				t.Fatalf("unexpected finding %+v", res.Findings[0])
			}
		})
	}
	// A clean prompt must not false-positive.
	if res, _, _ := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "an ordinary sentence with words")); len(res.Findings) != 0 {
		t.Fatalf("clean prompt false-positived: %+v", res.Findings)
	}
}

func TestEntropyDetector(t *testing.T) {
	eng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectEntropy: true}, nil)
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// A long high-entropy base64-ish token is flagged.
	high := "Zk9X3pQ7rT2vL8wB1nM6yH0sJ4dF5gK9aC2eR7uI3oP"
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "value "+high))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) == 0 || res.Findings[0].Detector != "entropy" {
		t.Fatalf("high-entropy token not flagged: %+v", res.Findings)
	}
	// Ordinary prose and a git-SHA-like hex string must NOT be flagged.
	for _, clean := range []string{
		"the quick brown fox jumps over the lazy dog repeatedly",
		"commit 356a192b7913b04c54574d18c28d46e6395428ab is fixed",
	} {
		if res, _, _ := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", clean)); len(res.Findings) != 0 {
			t.Fatalf("entropy false-positive on %q: %+v", clean, res.Findings)
		}
	}
}

func TestScan_OversizeSpanSkipped(t *testing.T) {
	eng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, MaxScanBytes: 32}, [][]byte{[]byte(testSecret)})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	big := strings.Repeat("A", 100) + testSecret
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", big))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !res.Skipped || res.SkipReason != "span_oversize" {
		t.Fatalf("expected span_oversize skip, got %+v", res)
	}
	if len(res.Findings) != 0 {
		t.Fatal("oversize span must not be scanned")
	}
}

func TestScan_ParseErrorFailOpenVsClosed(t *testing.T) {
	bad := []byte(`{not json`)

	open := newTestEngine(t, "block", testSecret) // OnScannerError default = pass
	res, _, err := open.ScanRequest(ChannelAnthropicMessages, bad)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if !res.Skipped || res.SkipReason != "parse_error" {
		t.Fatalf("expected parse_error skip, got %+v", res)
	}
	if open.ShouldBlock(res) {
		t.Fatal("fail-open: parse error must NOT block by default")
	}

	closed, err := NewEngine(types.LLMInspectionSpec{Mode: "block", DetectSecrets: true, OnScannerError: "block"}, [][]byte{[]byte(testSecret)})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	res2, _, _ := closed.ScanRequest(ChannelAnthropicMessages, bad)
	if !closed.ShouldBlock(res2) {
		t.Fatal("fail-closed: parse error must block when OnScannerError=block")
	}
}

func TestShouldBlock_ModeAndSeverity(t *testing.T) {
	alert := newTestEngine(t, "alert", testSecret)
	res, _, _ := alert.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "leak "+testSecret))
	if alert.ShouldBlock(res) {
		t.Fatal("alert mode must never block")
	}

	block := newTestEngine(t, "block", testSecret)
	res2, _, _ := block.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "leak "+testSecret))
	if !block.ShouldBlock(res2) {
		t.Fatal("block mode must block on a critical finding")
	}

	// blockMin above the finding severity -> no block.
	hi, err := NewEngine(types.LLMInspectionSpec{Mode: "block", DetectSecrets: true, BlockMinSeverity: "critical"}, [][]byte{[]byte(testSecret)})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	res3, _, _ := hi.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "leak "+testSecret))
	if !hi.ShouldBlock(res3) { // finding IS critical, so it still blocks
		t.Fatal("critical finding meets blockMin=critical")
	}
}

func TestScan_OpenAIChatExtractor(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)

	// content as a plain string in the last message.
	body := `{"model":"gpt-4","messages":[{"role":"user","content":` + jsonString("leak "+testSecret) + `}]}`
	if res, _, err := eng.ScanRequest(ChannelOpenAIChat, []byte(body)); err != nil || len(res.Findings) == 0 {
		t.Fatalf("openai content secret missed: err=%v findings=%+v", err, res.Findings)
	}

	// secret inside a tool_call's JSON-string arguments.
	args, _ := json.Marshal(`{"pw":"` + testSecret + `"}`)
	tc := `{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":` +
		`[{"type":"function","function":{"name":"db","arguments":` + string(args) + `}}]}]}`
	res, _, err := eng.ScanRequest(ChannelOpenAIChat, []byte(tc))
	if err != nil || len(res.Findings) == 0 {
		t.Fatalf("openai tool_call args secret missed: err=%v findings=%+v", err, res.Findings)
	}
	if !strings.Contains(res.Findings[0].FieldPath, "tool_calls[0].function.arguments") {
		t.Fatalf("unexpected field path %q", res.Findings[0].FieldPath)
	}

	// clean prompt -> nothing.
	clean := `{"model":"gpt-4","messages":[{"role":"user","content":"an ordinary clean prompt"}]}`
	if res, _, _ := eng.ScanRequest(ChannelOpenAIChat, []byte(clean)); len(res.Findings) != 0 {
		t.Fatalf("clean openai prompt should have no findings, got %+v", res.Findings)
	}
}

func TestPIIDetector(t *testing.T) {
	eng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectPII: true}, nil)
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	hits := map[string]string{
		"email":       "contact me at alice@example.com please",
		"us-ssn":      "ssn 123-45-6789 on file",
		"credit-card": "card 4242424242424242 expires soon", // valid Luhn
		"e164-phone":  "call +14155552671 now",
	}
	for name, text := range hits {
		t.Run(name, func(t *testing.T) {
			res, _, err := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", text))
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			found := false
			for _, f := range res.Findings {
				if f.Category == CategoryPII && f.Sample == maskedPlaceholder {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s not detected: %+v", name, res.Findings)
			}
		})
	}
	// Luhn rejects an invalid 16-digit number (no credit-card finding).
	res, _, _ := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "number 1234567812345678 here"))
	for _, f := range res.Findings {
		if f.Detector == "pii:credit-card" {
			t.Fatalf("Luhn must reject invalid card number, got %+v", f)
		}
	}
}

func TestSidecarDetector(t *testing.T) {
	// A fake sidecar that flags one PII span.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req sidecarRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(sidecarResponse{Findings: []sidecarFinding{
			{Type: "person", Category: "pii", Start: 0, End: 3, Severity: "medium"},
		}})
	}))
	defer srv.Close()

	eng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectorSidecarURL: srv.URL}, nil)
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "some prompt text"))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if calls == 0 {
		t.Fatal("sidecar was not called")
	}
	if len(res.Findings) == 0 || res.Findings[0].Detector != "sidecar:person" || res.Findings[0].Sample != maskedPlaceholder {
		t.Fatalf("sidecar finding not mapped content-free: %+v", res.Findings)
	}

	// Fail-OPEN: an unreachable sidecar yields no findings and no error.
	failEng, _ := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectorSidecarURL: "http://127.0.0.1:1"}, nil)
	res2, _, err2 := failEng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "prompt"))
	if err2 != nil {
		t.Fatalf("sidecar failure must fail-open, got err %v", err2)
	}
	if len(res2.Findings) != 0 {
		t.Fatalf("unreachable sidecar must yield no findings, got %+v", res2.Findings)
	}
}

func TestSidecarError_VisibleAsDegradedScan(t *testing.T) {
	// FIX #24: when the sidecar is the SOLE detector and it FAILS (here: a 500), the
	// scan was NOT completed. The Result must be marked Skipped/"sidecar_error" so an
	// alert-mode audit can tell this apart from a genuinely clean inspection — which
	// is otherwise byte-identical (Scanned:true, Findings:nil, Skipped:false).
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sidecar boom", http.StatusInternalServerError)
	}))
	defer down.Close()

	failEng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectorSidecarURL: down.URL}, nil)
	if err != nil || failEng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	res, _, err := failEng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "some prompt text"))
	// FAIL-OPEN behaviorally: traffic still flows (no ScanRequest error, no block).
	if err != nil {
		t.Fatalf("sidecar failure must fail-open (no ScanRequest error), got %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("a failed sidecar must yield no findings, got %+v", res.Findings)
	}
	// ...but the failure must be VISIBLE, not a false "clean".
	if !res.Skipped || res.SkipReason != "sidecar_error" {
		t.Fatalf("failed sidecar must be recorded as skipped/sidecar_error, got %+v", res)
	}
	// This engine is alert-mode, so it never blocks — visibility only. The block-mode
	// fail-open-by-default / fail-closed-under-OnScannerError=block behavior is covered
	// by TestSidecarError_RespectsOnScannerError below.
	if failEng.ShouldBlock(res) {
		t.Fatal("alert mode must never block")
	}

	// Contrast: a HEALTHY sidecar that returns zero findings is a genuinely clean
	// scan — NOT skipped — so the two states are distinguishable in the audit.
	clean := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(sidecarResponse{Findings: nil})
	}))
	defer clean.Close()
	cleanEng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectorSidecarURL: clean.URL}, nil)
	if err != nil || cleanEng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	cres, _, err := cleanEng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "some prompt text"))
	if err != nil {
		t.Fatalf("clean scan: %v", err)
	}
	if cres.Skipped || cres.SkipReason != "" {
		t.Fatalf("a clean sidecar scan must NOT be marked skipped, got %+v", cres)
	}
}

// TestSidecarError_RespectsOnScannerError proves the M2 fix: a sidecar outage IS a
// scanner error, so under block mode it fails OPEN by default but fails CLOSED when
// the operator set OnScannerError=block — matching the OnScannerError contract in
// types.go (an asymmetry would be code weaker than the documented claim).
func TestSidecarError_RespectsOnScannerError(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sidecar boom", http.StatusInternalServerError)
	}))
	defer down.Close()

	for _, tc := range []struct {
		name          string
		onScannerErr  string
		wantShouldBlk bool
	}{
		{"default_fail_open", "", false},
		{"explicit_block_fails_closed", "block", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, err := NewEngine(types.LLMInspectionSpec{
				Mode: "block", OnScannerError: tc.onScannerErr, DetectorSidecarURL: down.URL,
			}, nil)
			if err != nil || eng == nil {
				t.Fatalf("NewEngine: %v", err)
			}
			res, _, err := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "prompt"))
			if err != nil {
				t.Fatalf("scan must fail-open (no error): %v", err)
			}
			if !res.Skipped || res.SkipReason != "sidecar_error" {
				t.Fatalf("want skipped/sidecar_error, got %+v", res)
			}
			if got := eng.ShouldBlock(res); got != tc.wantShouldBlk {
				t.Fatalf("ShouldBlock=%v, want %v", got, tc.wantShouldBlk)
			}
		})
	}
}

// TestSidecarError_DoesNotClobberPriorSkip proves the M1 fix: SkipReason is shared
// across spans, so a later span's sidecar outage must NOT overwrite an earlier span's
// block-eligible span_oversize skip (which would silently downgrade a fail-closed
// outcome to fail-open under OnScannerError=block). The Anthropic walker yields the
// (oversize) system span before the (small) user span that hits the down sidecar.
func TestSidecarError_DoesNotClobberPriorSkip(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sidecar boom", http.StatusInternalServerError)
	}))
	defer down.Close()

	eng, err := NewEngine(types.LLMInspectionSpec{
		Mode: "block", OnScannerError: "block", DetectorSidecarURL: down.URL, MaxScanBytes: 20,
	}, nil)
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	oversizeSystem := strings.Repeat("A", 100) // > MaxScanBytes(20) => span_oversize, skips before sidecar
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, oversizeSystem, "hi"))
	if err != nil {
		t.Fatalf("scan must fail-open (no error): %v", err)
	}
	if res.SkipReason != "span_oversize" {
		t.Fatalf("sidecar outage must not clobber the earlier span_oversize skip, got %q", res.SkipReason)
	}
	if !eng.ShouldBlock(res) {
		t.Fatal("span_oversize under block+OnScannerError=block must block")
	}
}

func TestClassifyDetector(t *testing.T) {
	eng, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", ClassifiedMarkers: []string{"INTERNAL ONLY", "CONFIDENTIAL"}}, nil)
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	res, _, err := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "this doc is Internal Only, do not share"))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Findings) == 0 || res.Findings[0].Category != CategoryClassified {
		t.Fatalf("classified marker not detected: %+v", res.Findings)
	}
	if res.Findings[0].Sample != maskedPlaceholder {
		t.Fatalf("classified finding must be content-free")
	}
	if res2, _, _ := eng.ScanRequest(ChannelAnthropicMessages, anthropicBody(t, "", "ordinary public text")); len(res2.Findings) != 0 {
		t.Fatalf("unmarked text must not be flagged: %+v", res2.Findings)
	}
}

func TestGenericChannel(t *testing.T) {
	eng := newTestEngine(t, "alert", testSecret)
	// JSON connector body: a secret in any string leaf is found.
	jsonBody := []byte(`{"config":{"nested":{"token":"` + testSecret + `"}},"n":1}`)
	if res, _, err := eng.ScanRequest(ChannelGeneric, jsonBody); err != nil || len(res.Findings) == 0 {
		t.Fatalf("generic JSON leaf secret missed: err=%v findings=%+v", err, res.Findings)
	}
	// MCP/JSON-RPC body uses the same walk.
	mcp := []byte(`{"jsonrpc":"2.0","method":"tools/call","params":{"arguments":{"k":"` + testSecret + `"}}}`)
	if res, _, err := eng.ScanRequest(ChannelMCP, mcp); err != nil || len(res.Findings) == 0 {
		t.Fatalf("mcp jsonrpc secret missed: err=%v findings=%+v", err, res.Findings)
	}
	// Non-JSON body scanned as raw text.
	if res, _, err := eng.ScanRequest(ChannelGeneric, []byte("plain text with "+testSecret+" inside")); err != nil || len(res.Findings) == 0 {
		t.Fatalf("generic raw-text secret missed: err=%v findings=%+v", err, res.Findings)
	}
}

func TestScanAttachments(t *testing.T) {
	// A secret base64'd inside a document attachment is caught only when opted in.
	doc := base64.StdEncoding.EncodeToString([]byte("config: token=" + testSecret))
	body := []byte(`{"model":"c","max_tokens":1,"messages":[{"role":"user","content":` +
		`[{"type":"document","source":{"type":"base64","data":"` + doc + `"}}]}]}`)

	off := newTestEngine(t, "alert", testSecret) // ScanAttachments false
	if res, _, _ := off.ScanRequest(ChannelAnthropicMessages, body); len(res.Findings) != 0 {
		t.Fatalf("attachment must NOT be scanned by default, got %+v", res.Findings)
	}
	on, err := NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, ScanAttachments: true}, [][]byte{[]byte(testSecret)})
	if err != nil || on == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	res, _, err := on.ScanRequest(ChannelAnthropicMessages, body)
	if err != nil || len(res.Findings) == 0 {
		t.Fatalf("opt-in attachment scan missed the secret: err=%v findings=%+v", err, res.Findings)
	}
	if !strings.Contains(res.Findings[0].FieldPath, "source.data(decoded)") {
		t.Fatalf("unexpected attachment field path %q", res.Findings[0].FieldPath)
	}
}

func TestNilEngineSafe(t *testing.T) {
	var eng *Engine
	res, out, err := eng.ScanRequest(ChannelAnthropicMessages, []byte(`{}`))
	if err != nil || res.Scanned || eng.Mode() != ModeOff || eng.ShouldBlock(res) {
		t.Fatalf("nil engine must be a safe no-op: %+v %v", res, err)
	}
	_ = out
}
