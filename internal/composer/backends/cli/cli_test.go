// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/composer/backends/composertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assertValidProposal checks the parsed Proposal matches composertest.ValidProposalJSON,
// plus the cli-specific second grant (an api_key alongside the github_token).
func assertValidProposal(t *testing.T, p composer.Proposal) {
	t.Helper()
	composertest.AssertValidProposal(t, p)
	if got := len(p.InlinePolicy.EligibleGrants); got != 2 {
		t.Fatalf("EligibleGrants len = %d, want 2", got)
	}
	if p.InlinePolicy.EligibleGrants[1].Kind != types.GrantAPIKey {
		t.Errorf("grant[1].Kind = %q, want api_key", p.InlinePolicy.EligibleGrants[1].Kind)
	}
}

// --- fake CLI scaffolding ----------------------------------------------------

// argvDumpPath is the file each fake CLI writes its argv to so the test can
// assert the backend passed the expected flags. It lives in the test's temp dir.
type fakeCLI struct {
	bin     string // path to the fake executable
	argvLog string // path the fake wrote its argv to
}

// writeFakeCLI writes an executable bash script to a temp dir and returns its
// paths. body is the script body (after a shared preamble that records argv). The
// preamble writes every argument on its own line to $WARDYN_ARGV_LOG.
func writeFakeCLI(t *testing.T, name, body string) fakeCLI {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, name)
	argvLog := filepath.Join(dir, "argv.log")

	// Record each argument NUL-separated: arguments themselves contain newlines
	// (the assembled user message / system prompt), so a newline separator would
	// corrupt the log. NUL cannot appear in an argv entry.
	script := "#!/usr/bin/env bash\n" +
		"set -u\n" +
		": > \"" + argvLog + "\"\n" +
		"for a in \"$@\"; do printf '%s\\0' \"$a\" >> \"" + argvLog + "\"; done\n" +
		body + "\n"

	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake CLI: %v", err)
	}
	return fakeCLI{bin: bin, argvLog: argvLog}
}

// argv returns the recorded arguments (NUL-separated) the backend invoked.
func (f fakeCLI) argv(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	s := strings.TrimRight(string(b), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// flagValue returns the argument following the named flag in argv, or "".
func flagValue(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// --- claude tool -------------------------------------------------------------

func TestClaude_HappyPath(t *testing.T) {
	// Fake claude: emit a JSON wrapper whose .structured_output is the proposal.
	// claude's --json-schema is passed INLINE (not a file), so no copy step.
	fake := writeFakeCLI(t, "claude",
		`printf '%s' '{"type":"result","is_error":false,"structured_output":`+composertest.ValidProposalJSON+`}'`)

	c, err := NewComposer(Config{Tool: ToolClaude, Model: "claude-sonnet-4-5", BinPath: fake.bin})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}

	p, err := c.Propose(context.Background(), composertest.SampleRequest())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	assertValidProposal(t, p)

	argv := fake.argv(t)

	// -p <userMessage> carries the assembled BuildUserMessage content.
	user, ok := flagValue(argv, "-p")
	if !ok {
		t.Fatalf("argv missing -p flag: %v", argv)
	}
	wantUser := composer.BuildUserMessage(composertest.SampleRequest())
	if user != wantUser {
		t.Errorf("-p value mismatch:\n got %q\nwant %q", user, wantUser)
	}

	// --output-format json
	if v, _ := flagValue(argv, "--output-format"); v != "json" {
		t.Errorf("--output-format = %q, want json", v)
	}
	// --model from cfg.
	if v, _ := flagValue(argv, "--model"); v != "claude-sonnet-4-5" {
		t.Errorf("--model = %q, want claude-sonnet-4-5", v)
	}
	// --max-turns is the small slack cap (a capable model may take an extra
	// thinking/read-only-tool turn before emitting the JSON; --max-turns 1 flaked).
	if v, _ := flagValue(argv, "--max-turns"); v != composerMaxTurns {
		t.Errorf("--max-turns = %q, want %s", v, composerMaxTurns)
	}
	// --append-system-prompt carries SystemPrompt().
	if v, _ := flagValue(argv, "--append-system-prompt"); v != composer.SystemPrompt() {
		t.Errorf("--append-system-prompt mismatch:\n got %q\nwant %q", v, composer.SystemPrompt())
	}
	// --bare must NOT be passed.
	if slices.Contains(argv, "--bare") {
		t.Error("argv unexpectedly contains --bare")
	}
	// --json-schema carries the portable strict schema INLINE as JSON (claude
	// parses the value itself; it is NOT a file path — that was a real bug: claude
	// 2.1.195 rejects a path with "--json-schema is not valid JSON").
	schemaArg, ok := flagValue(argv, "--json-schema")
	if !ok {
		t.Fatalf("argv missing --json-schema: %v", argv)
	}
	var gotSchema map[string]any
	if err := json.Unmarshal([]byte(schemaArg), &gotSchema); err != nil {
		t.Fatalf("--json-schema must be inline JSON, got %q: %v", schemaArg, err)
	}
	props, _ := gotSchema["properties"].(map[string]any)
	for _, want := range []string{"run", "inline_policy", "summary", "warnings"} {
		if _, ok := props[want]; !ok {
			t.Errorf("inline --json-schema missing property %q (props=%v)", want, props)
		}
	}
}

func TestClaude_LeastPrivilegePermissionMode(t *testing.T) {
	// FIX #11: the claude path runs as a subprocess of wardynd on the control-plane
	// HOST (not in a CC sandbox). It must pass --permission-mode plan so Claude Code
	// runs read-only and cannot execute a tool — the read-only parity to codex's
	// --sandbox read-only. Otherwise a prompt-injected attachment could induce a tool
	// call and get host code execution based solely on the CLI's ambient default.
	fake := writeFakeCLI(t, "claude",
		`printf '%s' '{"is_error":false,"structured_output":`+composertest.ValidProposalJSON+`}'`)
	c, err := NewComposer(Config{Tool: ToolClaude, BinPath: fake.bin})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	if _, err := c.Propose(context.Background(), composertest.SampleRequest()); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	argv := fake.argv(t)
	if v, _ := flagValue(argv, "--permission-mode"); v != "plan" {
		t.Errorf("--permission-mode = %q, want plan (read-only parity with codex)", v)
	}
	// The OPPOSITE flag must never be present.
	if slices.Contains(argv, "--dangerously-skip-permissions") || slices.Contains(argv, "--allow-dangerously-skip-permissions") {
		t.Error("argv must NOT bypass permissions on the host claude path")
	}
}

func TestClaude_ScrubsAnthropicAPIKey(t *testing.T) {
	// The fake records its environment so we can assert ANTHROPIC_API_KEY is gone
	// (subscription auth) while an unrelated var survives.
	dir := t.TempDir()
	envLog := filepath.Join(dir, "env.log")
	fake := writeFakeCLI(t, "claude",
		`env > "`+envLog+`"; printf '%s' '{"is_error":false,"structured_output":`+composertest.ValidProposalJSON+`}'`)

	t.Setenv("ANTHROPIC_API_KEY", "sk-should-be-scrubbed")
	t.Setenv("WARDYN_FAKE_MARKER", "kept")

	c, err := NewComposer(Config{Tool: ToolClaude, BinPath: fake.bin})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	if _, err := c.Propose(context.Background(), composertest.SampleRequest()); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	b, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	childEnv := string(b)
	if strings.Contains(childEnv, "ANTHROPIC_API_KEY=") {
		t.Errorf("child env still contains ANTHROPIC_API_KEY:\n%s", childEnv)
	}
	if !strings.Contains(childEnv, "WARDYN_FAKE_MARKER=kept") {
		t.Errorf("child env lost unrelated var WARDYN_FAKE_MARKER:\n%s", childEnv)
	}
}

func TestClaude_OmitsModelWhenEmpty(t *testing.T) {
	fake := writeFakeCLI(t, "claude",
		`printf '%s' '{"is_error":false,"structured_output":`+composertest.ValidProposalJSON+`}'`)
	c, err := NewComposer(Config{Tool: ToolClaude, BinPath: fake.bin})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	if _, err := c.Propose(context.Background(), composertest.SampleRequest()); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if slices.Contains(fake.argv(t), "--model") {
		t.Error("argv unexpectedly contains --model when cfg.Model is empty")
	}
}

func TestClaude_ReportedError(t *testing.T) {
	// A wrapper with is_error=true and no structured_output is a clean backend
	// error (returned immediately by ProposeWithRetry, not retried).
	fake := writeFakeCLI(t, "claude",
		`printf '%s' '{"is_error":true,"error":"model refused"}'`)
	c, err := NewComposer(Config{Tool: ToolClaude, BinPath: fake.bin, MaxAttempts: 2})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	_, err = c.Propose(context.Background(), composertest.SampleRequest())
	if err == nil {
		t.Fatal("expected error from is_error wrapper, got nil")
	}
	if !strings.Contains(err.Error(), "model refused") {
		t.Errorf("error = %v, want it to mention the reported error", err)
	}
}

// --- codex tool --------------------------------------------------------------

func TestCodex_HappyPath(t *testing.T) {
	// Fake codex: copy the --output-schema file aside, then find its -o argument
	// and write the proposal JSON to that file.
	fake := writeFakeCLI(t, "codex",
		copySchemaBody("--output-schema")+codexWriteOutBody(composertest.ValidProposalJSON))

	c, err := NewComposer(Config{Tool: ToolCodex, Model: "gpt-5-codex", BinPath: fake.bin})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}

	p, err := c.Propose(context.Background(), composertest.SampleRequest())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	assertValidProposal(t, p)

	argv := fake.argv(t)
	if !slices.Contains(argv, "exec") {
		t.Errorf("argv missing 'exec' subcommand: %v", argv)
	}
	if v, _ := flagValue(argv, "--sandbox"); v != "read-only" {
		t.Errorf("--sandbox = %q, want read-only", v)
	}
	if v, _ := flagValue(argv, "--ask-for-approval"); v != "never" {
		t.Errorf("--ask-for-approval = %q, want never", v)
	}
	if v, _ := flagValue(argv, "-m"); v != "gpt-5-codex" {
		t.Errorf("-m = %q, want gpt-5-codex", v)
	}
	// -o points at an output file; --output-schema at the portable schema file.
	if _, ok := flagValue(argv, "-o"); !ok {
		t.Errorf("argv missing -o output file: %v", argv)
	}
	schemaPath, ok := flagValue(argv, "--output-schema")
	if !ok {
		t.Fatalf("argv missing --output-schema: %v", argv)
	}
	assertSchemaPath(t, schemaPath)
	verifySchemaContents(t, filepath.Join(filepath.Dir(fake.bin), "schema-copy.json"))
	// codex exec has no separate system-prompt channel, so the final positional
	// argument is the system prompt prepended to the fenced user message.
	wantPrompt := composer.SystemPrompt() + "\n\n" + composer.BuildUserMessage(composertest.SampleRequest())
	if last := argv[len(argv)-1]; last != wantPrompt {
		t.Errorf("final positional arg mismatch:\n got %q\nwant %q", last, wantPrompt)
	}
}

func TestClaude_Clarify(t *testing.T) {
	// Fake claude: emit a wrapper whose .structured_output is a clarification.
	clarJSON := `{"ready":false,"questions":[{"id":"gh","question":"Push access?","why":"scope token","options":["read","write"],"multi":false}],"assumptions":[],"notes":""}`
	fake := writeFakeCLI(t, "claude",
		`printf '%s' '{"type":"result","is_error":false,"structured_output":`+clarJSON+`}'`)

	c, err := NewComposer(Config{Tool: ToolClaude, Model: "claude-sonnet-4-5", BinPath: fake.bin})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	clr, ok := c.(composer.Clarifier)
	if !ok {
		t.Fatal("cli backend must implement composer.Clarifier")
	}

	cl, err := clr.Clarify(context.Background(), composertest.SampleRequest())
	if err != nil {
		t.Fatalf("Clarify: %v", err)
	}
	if cl.Ready || len(cl.Questions) != 1 || cl.Questions[0].ID != "gh" {
		t.Errorf("clarification mapped wrong: %+v", cl)
	}

	// The clarify path must pass the CLARIFY system prompt + the clarify schema.
	argv := fake.argv(t)
	if v, _ := flagValue(argv, "--append-system-prompt"); v != composer.ClarifySystemPrompt() {
		t.Errorf("--append-system-prompt should be the CLARIFY prompt")
	}
	if v, _ := flagValue(argv, "--json-schema"); !strings.Contains(v, `"ready"`) {
		t.Errorf("--json-schema should be the clarification schema, got %q", v)
	}
}

func TestCodex_EmptyOutputFileErrors(t *testing.T) {
	// Codex exits 0 but writes nothing -> retried then fail-closed.
	fake := writeFakeCLI(t, "codex", `exit 0`)
	c, err := NewComposer(Config{Tool: ToolCodex, BinPath: fake.bin, MaxAttempts: 2})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	if _, err := c.Propose(context.Background(), composertest.SampleRequest()); err == nil {
		t.Fatal("expected error on empty codex output, got nil")
	}
}

// codexWriteOutBody returns a script body that locates the -o argument and writes
// proposal to that file.
func codexWriteOutBody(proposal string) string {
	return `out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
printf '%s' '` + proposal + `' > "$out"`
}

// --- retry / fail-closed -----------------------------------------------------

func TestRetry_RecoversAfterMalformed(t *testing.T) {
	// First attempt emits malformed JSON, second emits a valid proposal. Drive it
	// with a counter file so the fake behaves differently per attempt.
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	body := `n=0
[ -f "` + counter + `" ] && n=$(cat "` + counter + `")
n=$((n+1)); printf '%s' "$n" > "` + counter + `"
if [ "$n" -lt 2 ]; then
  printf '%s' 'not json at all {{{'
else
  printf '%s' '{"is_error":false,"structured_output":` + composertest.ValidProposalJSON + `}'
fi`
	bin := filepath.Join(dir, "claude")
	script := "#!/usr/bin/env bash\nset -u\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}

	c, err := NewComposer(Config{Tool: ToolClaude, BinPath: bin, MaxAttempts: 3})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	p, err := c.Propose(context.Background(), composertest.SampleRequest())
	if err != nil {
		t.Fatalf("Propose after retry: %v", err)
	}
	assertValidProposal(t, p)

	got, _ := os.ReadFile(counter)
	if strings.TrimSpace(string(got)) != "2" {
		t.Errorf("expected exactly 2 attempts, counter=%q", got)
	}
}

func TestRetry_FailsClosedOnPersistentMalformed(t *testing.T) {
	// structured_output present but not a valid proposal (wrong shape) -> every
	// attempt fails validation -> fail closed after MaxAttempts.
	fake := writeFakeCLI(t, "claude",
		`printf '%s' '{"is_error":false,"structured_output":{"run":{"agent":42}}}'`)
	c, err := NewComposer(Config{Tool: ToolClaude, BinPath: fake.bin, MaxAttempts: 2})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	_, err = c.Propose(context.Background(), composertest.SampleRequest())
	if err == nil {
		t.Fatal("expected fail-closed error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid output after") {
		t.Errorf("error = %v, want it to mention fail-closed after attempts", err)
	}
}

func TestClaude_MissingStructuredOutputFieldRetries(t *testing.T) {
	// A wrapper with no structured_output and no is_error is an invalid attempt.
	fake := writeFakeCLI(t, "claude", `printf '%s' '{"type":"result","is_error":false}'`)
	c, err := NewComposer(Config{Tool: ToolClaude, BinPath: fake.bin, MaxAttempts: 2})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	if _, err := c.Propose(context.Background(), composertest.SampleRequest()); err == nil {
		t.Fatal("expected error for missing structured_output, got nil")
	}
}

// --- missing binary ----------------------------------------------------------

func TestMissingBinary_ErrorsCleanly(t *testing.T) {
	c, err := NewComposer(Config{Tool: ToolClaude, BinPath: filepath.Join(t.TempDir(), "does-not-exist")})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	_, err = c.Propose(context.Background(), composertest.SampleRequest())
	if err == nil {
		t.Fatal("expected error for missing binary, got nil")
	}
	if !strings.Contains(err.Error(), "not found or not executable") {
		t.Errorf("error = %v, want a clear missing-binary message", err)
	}
}

// --- timeout -----------------------------------------------------------------

func TestTimeout_KillsHangingCLI(t *testing.T) {
	// A fake that sleeps far longer than the configured timeout must be killed and
	// reported as a timeout (not a generic exit error).
	fake := writeFakeCLI(t, "claude", `sleep 30; printf '%s' '{}'`)
	c, err := NewComposer(Config{Tool: ToolClaude, BinPath: fake.bin, Timeout: 200 * time.Millisecond, MaxAttempts: 1})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}

	start := time.Now()
	_, err = c.Propose(context.Background(), composertest.SampleRequest())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want it to mention timeout", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Propose took %s; timeout did not kill the process promptly", elapsed)
	}
}

func TestContextCancel_PropagatesPromptly(t *testing.T) {
	fake := writeFakeCLI(t, "claude", `sleep 30; printf '%s' '{}'`)
	c, err := NewComposer(Config{Tool: ToolClaude, BinPath: fake.bin, Timeout: 30 * time.Second, MaxAttempts: 1})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err = c.Propose(ctx, composertest.SampleRequest())
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took %s; ctx was not honored promptly", elapsed)
	}
}

// --- config validation -------------------------------------------------------

func TestNewComposer_ConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "claude ok", cfg: Config{Tool: ToolClaude}},
		{name: "codex ok", cfg: Config{Tool: ToolCodex}},
		{name: "tool with whitespace ok", cfg: Config{Tool: "  claude  "}},
		{name: "empty tool", cfg: Config{Tool: ""}, wantErr: true},
		{name: "unknown tool", cfg: Config{Tool: "gemini"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewComposer(tt.cfg)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestNewComposer_Defaults(t *testing.T) {
	c, err := NewComposer(Config{Tool: ToolClaude})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	cc, ok := c.(*cliComposer)
	if !ok {
		t.Fatalf("NewComposer returned %T, want *cliComposer", c)
	}
	if cc.binPath != ToolClaude {
		t.Errorf("default binPath = %q, want %q", cc.binPath, ToolClaude)
	}
	if cc.timeout != defaultTimeout {
		t.Errorf("default timeout = %s, want %s", cc.timeout, defaultTimeout)
	}
}

func TestValidateRequest_RejectsEmpty(t *testing.T) {
	// An empty request is rejected before any CLI invocation: point at a binary
	// that would fail loudly if ever run.
	fake := writeFakeCLI(t, "claude", `echo SHOULD_NOT_RUN >&2; exit 3`)
	c, err := NewComposer(Config{Tool: ToolClaude, BinPath: fake.bin})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	if _, err := c.Propose(context.Background(), composer.ComposeRequest{}); err == nil {
		t.Fatal("expected validation error for empty request, got nil")
	}
	if _, statErr := os.Stat(fake.argvLog); statErr == nil {
		t.Error("CLI was invoked for an invalid request; it should be rejected first")
	}
}

// --- schema file assertion ---------------------------------------------------

// copySchemaBody returns a script fragment that copies the file named by the
// given schema flag to "<dir-of-this-script>/schema-copy.json" before producing
// output. run() removes the temp schema file on return, so the fake must capture
// its contents while it is still present.
func copySchemaBody(schemaFlag string) string {
	return `prev=""
for a in "$@"; do
  if [ "$prev" = "` + schemaFlag + `" ]; then cp "$a" "$(dirname "$0")/schema-copy.json"; fi
  prev="$a"
done
`
}

// assertSchemaPath checks the path the backend passed is an absolute,
// schema-named temp path.
func assertSchemaPath(t *testing.T, path string) {
	t.Helper()
	if !filepath.IsAbs(path) {
		t.Errorf("schema file path %q is not absolute", path)
	}
	if !strings.Contains(filepath.Base(path), composer.ProposalSchemaName) {
		t.Errorf("schema file %q does not carry the schema name %q", path, composer.ProposalSchemaName)
	}
}

// verifySchemaContents parses a schema file and asserts it is the portable
// proposal schema (strict object exposing run + inline_policy).
func verifySchemaContents(t *testing.T, copyPath string) {
	t.Helper()
	raw, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("read schema copy: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if got["type"] != "object" {
		t.Errorf("schema type = %v, want object", got["type"])
	}
	if got["additionalProperties"] != false {
		t.Errorf("schema additionalProperties = %v, want false (strict)", got["additionalProperties"])
	}
	props, _ := got["properties"].(map[string]any)
	for _, key := range []string{"run", "inline_policy", "summary", "warnings"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema missing top-level property %q", key)
		}
	}
}
