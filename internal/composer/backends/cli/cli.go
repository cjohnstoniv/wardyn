// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package cli implements a composer.Composer backed by the operator's resident
// coding-agent CLI (Claude Code or Codex) running under its own logged-in
// SUBSCRIPTION — no API key is minted or passed. It shells out to the CLI with a
// schema-forcing structured-output mode, extracts the schema-valid JSON object
// the CLI emits, and hands it to composer.ProposeWithRetry for the canonical
// parse/validate/bounded-retry/fail-closed loop.
//
// Trust model: the child runs on the CONTROL-PLANE host against UNTRUSTED input
// (task text + attachments), so it must be able to take NO action against the host.
// Wardyn enforces this per tool with explicit least-privilege flags, NOT the CLI's
// ambient default: codex gets `--sandbox read-only --ask-for-approval never` (which
// also blocks the network); claude gets `--permission-mode plan` PLUS an explicit
// `--disallowedTools` denylist (composerDisallowedTools) — plan mode ALONE still
// permits read-only tools including WebFetch/WebSearch (network), host file reads,
// and any resident MCP tool, so the denylist PLUS `--strict-mcp-config` (which drops
// every operator MCP server) is what actually closes host file-exfiltration / SSRF from
// a prompt-injected attachment (H12). The CLI is used purely as a structured-output
// text generator; its output is Grade+Clamped downstream. ANTHROPIC_API_KEY is
// scrubbed from the child env for the claude tool so it uses the subscription
// session (never an API key) and never bills/leaks one.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/composer"
)

// Tool identifies which resident CLI to shell out to.
const (
	ToolClaude = "claude"
	ToolCodex  = "codex"
)

// defaultTimeout bounds a single CLI invocation when cfg.Timeout is unset.
const defaultTimeout = 120 * time.Second

// composerMaxTurns bounds the CLI's agent loop. The composer only needs the model
// to EMIT a JSON proposal, but a capable model (e.g. Opus) NON-DETERMINISTICALLY
// spends an extra thinking / read-only-tool turn before emitting — and --max-turns 1
// then fails the ENTIRE compose with error_max_turns (surfaced as "the composer
// backend failed to respond"). A small slack absorbs that jitter. It is safe: the
// invocation runs --permission-mode plan, so no writes or command execution can
// happen regardless of turn count, and the per-invocation timeout is the real
// backstop on runaway analysis.
const composerMaxTurns = "6"

// composerDisallowedTools denies EVERY built-in tool for the claude invocations. The
// composer uses the CLI purely as a structured-output text generator — it needs
// ZERO tools. `--permission-mode plan` blocks MUTATING tools but still permits
// read-only ones — file Read/Glob/Grep and, crucially, WebFetch/WebSearch network
// reads — so a prompt-injected attachment run on the CONTROL-PLANE host could
// exfiltrate host file contents or reach the network. Denying the tools outright
// closes that (H12). The operator's MCP tools are a separate, unbounded class an
// enumerated denylist can't cover: the claude invocations also pass
// `--strict-mcp-config` with no `--mcp-config`, so NO MCP servers load and there
// are no mcp__* tools to call. (codex needs no equivalent: `--sandbox read-only` fully
// sandboxes it, blocking both writes and the network.)
//
// ponytail: enumerated built-in denylist; a NEW built-in tool must be added here.
// The unbounded operator-MCP class is instead closed structurally by --strict-mcp-config.
const composerDisallowedTools = "Read,Glob,Grep,Bash,BashOutput,KillShell,Edit,Write,NotebookEdit,WebFetch,WebSearch,Task,TodoWrite,Skill,SlashCommand,ToolSearch"

// Config configures the CLI composer backend.
type Config struct {
	// Tool selects the resident CLI: "claude" (Claude Code) or "codex".
	Tool string
	// Model is the model id passed to the CLI (e.g. "claude-sonnet-4-5",
	// "gpt-5"). Empty lets the CLI use its own configured default.
	Model string
	// BinPath is the path to the CLI binary. Empty defaults to the tool name
	// ("claude" / "codex"), resolved against PATH.
	BinPath string
	// Timeout bounds a single CLI invocation. Zero uses defaultTimeout.
	Timeout time.Duration
	// MaxAttempts bounds the parse/validate/retry loop. <1 uses
	// composer.DefaultMaxAttempts.
	MaxAttempts int
}

// cliComposer is the resident-CLI Composer implementation.
type cliComposer struct {
	tool        string
	model       string
	binPath     string
	timeout     time.Duration
	maxAttempts int
}

// NewComposer validates cfg and returns a Composer that shells out to the
// operator's resident subscription CLI. It does NOT verify the binary exists at
// construction time (the operator may install it later / it may live only in the
// daemon's PATH); a missing binary surfaces as a clear error on Propose.
func NewComposer(cfg Config) (composer.Composer, error) {
	tool := strings.TrimSpace(cfg.Tool)
	switch tool {
	case ToolClaude, ToolCodex:
	case "":
		return nil, errors.New("cli composer: Tool is required (\"claude\" or \"codex\")")
	default:
		return nil, fmt.Errorf("cli composer: unknown Tool %q (want \"claude\" or \"codex\")", tool)
	}

	bin := strings.TrimSpace(cfg.BinPath)
	if bin == "" {
		bin = tool
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	return &cliComposer{
		tool:        tool,
		model:       strings.TrimSpace(cfg.Model),
		binPath:     bin,
		timeout:     timeout,
		maxAttempts: cfg.MaxAttempts,
	}, nil
}

// Propose runs the resident CLI once per attempt via composer.ProposeWithRetry.
func (c *cliComposer) Propose(ctx context.Context, req composer.ComposeRequest) (composer.Proposal, error) {
	if err := composer.ValidateRequest(req); err != nil {
		return composer.Proposal{}, err
	}
	schema := composer.ProposalJSONSchema()
	system := composer.SystemPrompt()
	user := composer.BuildUserMessage(req)

	// Write the JSON Schema to a temp file once; every attempt reuses it (codex
	// reads it via --output-schema; claude takes it inline).
	schemaFile, cleanup, err := writeSchemaFile(schema, composer.ProposalSchemaName)
	if err != nil {
		return composer.Proposal{}, err
	}
	defer cleanup()

	return composer.ProposeWithRetry(ctx, c.maxAttempts, func(ctx context.Context, _ int) ([]byte, error) {
		return c.run(ctx, schema, schemaFile, system, user)
	})
}

// Clarify runs the SAME CLI wire with the clarify schema + prompt.
func (c *cliComposer) Clarify(ctx context.Context, req composer.ComposeRequest) (composer.Clarification, error) {
	if err := composer.ValidateRequest(req); err != nil {
		return composer.Clarification{}, err
	}
	schema := composer.ClarificationJSONSchema()
	system := composer.ClarifySystemPrompt()
	user := composer.BuildUserMessage(req)

	schemaFile, cleanup, err := writeSchemaFile(schema, composer.ClarifySchemaName)
	if err != nil {
		return composer.Clarification{}, err
	}
	defer cleanup()

	return composer.ClarifyWithRetry(ctx, c.maxAttempts, func(ctx context.Context, _ int) ([]byte, error) {
		return c.run(ctx, schema, schemaFile, system, user)
	})
}

// Assist answers ONE plain-language operator question about the proposed setup as
// INERT advisory free text. It shells out to the SAME resident CLI single-shot but
// with NO structured-output flag (claude: no --json-schema; codex: no
// --output-schema), so the CLI returns prose rather than a schema object. One turn,
// read-only, approvals disabled — same trust posture as Propose/Clarify. The answer
// is never re-graded or clamped.
func (c *cliComposer) Assist(ctx context.Context, req composer.ComposeRequest, question string) (string, error) {
	if err := composer.ValidateRequest(req); err != nil {
		return "", err
	}
	system := composer.AssistSystemPrompt
	user := composer.AssistUserMessage(req, question)

	runCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	switch c.tool {
	case ToolClaude:
		return c.assistClaude(runCtx, system, user)
	case ToolCodex:
		return c.assistCodex(runCtx, system, user)
	default:
		// Unreachable: NewComposer validates the tool.
		return "", fmt.Errorf("cli composer: unknown tool %q", c.tool)
	}
}

// assistClaude runs Claude Code in headless print mode WITHOUT --json-schema, so it
// returns free prose. The answer lands in the JSON wrapper's ".result" field.
// `--permission-mode plan` runs the CLI read-only (no mutating tools or command
// execution; read-only tool use may still occur, bounded by a small --max-turns cap) — the same
// least-privilege posture as runClaude, so a prompt-injected input cannot make the
// host CLI take a mutating action.
func (c *cliComposer) assistClaude(ctx context.Context, system, user string) (string, error) {
	args := []string{
		"-p", user,
		"--output-format", "json",
		"--append-system-prompt", system,
		"--permission-mode", "plan",
		"--strict-mcp-config", // no --mcp-config below => load ZERO operator MCP servers
		"--disallowedTools", composerDisallowedTools,
		"--max-turns", composerMaxTurns,
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	out, err := c.exec(ctx, scrubAnthropicKey(os.Environ()), args...)
	if err != nil {
		return "", err
	}
	var wrapper struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Error   string `json:"error"`
	}
	if jerr := json.Unmarshal(out, &wrapper); jerr != nil {
		return "", fmt.Errorf("cli composer: claude assist output was not the expected JSON wrapper: %w", jerr)
	}
	if wrapper.IsError {
		msg := strings.TrimSpace(wrapper.Error)
		if msg == "" {
			msg = "claude reported is_error with no message"
		}
		return "", fmt.Errorf("cli composer: claude assist reported error: %s", msg)
	}
	ans := strings.TrimSpace(wrapper.Result)
	if ans == "" {
		return "", errors.New("cli composer: claude assist produced no answer text")
	}
	return ans, nil
}

// assistCodex runs Codex non-interactively WITHOUT --output-schema; the final
// assistant message is written to the -o file, which we read as plain text.
func (c *cliComposer) assistCodex(ctx context.Context, system, user string) (string, error) {
	outPath, cleanup, err := tempOutputFile("wardyn-codex-assist-*.txt")
	if err != nil {
		return "", err
	}
	defer cleanup()

	prompt := user
	if strings.TrimSpace(system) != "" {
		prompt = system + "\n\n" + user
	}
	args := []string{
		"exec",
		"-o", outPath,
		"--sandbox", "read-only",
		"--ask-for-approval", "never",
	}
	if c.model != "" {
		args = append(args, "-m", c.model)
	}
	args = append(args, prompt)

	if _, err := c.exec(ctx, os.Environ(), args...); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		return "", fmt.Errorf("cli composer: read codex output file: %w", err)
	}
	ans := strings.TrimSpace(string(raw))
	if ans == "" {
		return "", errors.New("cli composer: codex assist produced no answer text")
	}
	return ans, nil
}

// run performs ONE CLI invocation and returns the raw schema-valid JSON object
// the CLI produced (the bytes the foundation parses). Each attempt re-runs the
// process under its own timeout-bounded context.
func (c *cliComposer) run(ctx context.Context, schema map[string]any, schemaFile, system, user string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	switch c.tool {
	case ToolClaude:
		return c.runClaude(runCtx, schema, system, user)
	case ToolCodex:
		return c.runCodex(runCtx, schemaFile, system, user)
	default:
		// Unreachable: NewComposer validates the tool.
		return nil, fmt.Errorf("cli composer: unknown tool %q", c.tool)
	}
}

// runClaude invokes Claude Code in headless print mode with structured output:
//
//	claude -p <user> --model <model> --output-format json
//	  --json-schema '<schema JSON>' --append-system-prompt <system>
//	  --permission-mode plan --max-turns 1
//
// `--permission-mode plan` is the read-only parity to codex's `--sandbox read-only
// --ask-for-approval never`: plan mode disables all MUTATING tools (edits, writes,
// command execution), so a prompt-injected attachment cannot get the host CLI to take
// an action against the host. Read-only tool use may still occur but is bounded by
// --max-turns 1; the composer only needs the model to EMIT a JSON proposal, which is
// Grade+Clamped downstream regardless.
//
// IMPORTANT: claude's --json-schema takes the schema as an INLINE JSON STRING
// (not a file path — that is codex's --output-schema). The schema-valid object
// lands in the JSON wrapper's ".structured_output" field; we extract THAT as the
// raw bytes. ANTHROPIC_API_KEY is scrubbed from the child env so the CLI uses the
// resident subscription rather than an API key.
func (c *cliComposer) runClaude(ctx context.Context, schema map[string]any, system, user string) ([]byte, error) {
	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("cli composer: marshal schema: %w", err)
	}
	args := []string{
		"-p", user,
		"--output-format", "json",
		"--json-schema", string(schemaJSON),
		"--append-system-prompt", system,
		"--permission-mode", "plan",
		"--strict-mcp-config", // no --mcp-config below => load ZERO operator MCP servers
		"--disallowedTools", composerDisallowedTools,
		"--max-turns", composerMaxTurns,
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}

	out, err := c.exec(ctx, scrubAnthropicKey(os.Environ()), args...)
	if err != nil {
		return nil, err // transport/process failure — returned immediately (not retried)
	}

	var wrapper struct {
		StructuredOutput json.RawMessage `json:"structured_output"`
		IsError          bool            `json:"is_error"`
		Error            string          `json:"error"`
	}
	if jerr := json.Unmarshal(out, &wrapper); jerr != nil {
		// A non-wrapper response is malformed MODEL OUTPUT, not a transport error:
		// hand the raw bytes to ParseProposal so the canonical loop RETRIES it
		// (ParseProposal will fail closed on garbage).
		return out, nil
	}
	if wrapper.IsError {
		// The CLI explicitly reported a failure (auth, rate-limit, refusal): a
		// real backend error — return it immediately rather than retrying garbage.
		msg := strings.TrimSpace(wrapper.Error)
		if msg == "" {
			msg = "claude reported is_error with no message"
		}
		return nil, fmt.Errorf("cli composer: claude reported error: %s", msg)
	}
	if len(wrapper.StructuredOutput) == 0 || string(wrapper.StructuredOutput) == "null" {
		// Wrapper present but no schema object: a malformed attempt — return bytes
		// that fail ParseProposal so the loop retries / fails closed.
		return wrapper.StructuredOutput, nil
	}
	return wrapper.StructuredOutput, nil
}

// runCodex invokes Codex non-interactively with an output schema, reading the
// final schema-valid message from the -o output file:
//
//	codex exec --output-schema <schemaFile> -o <outFile> --sandbox read-only
//	  --ask-for-approval never -m <model> <prompt>
//
// codex exec has no separate system-prompt channel, so the Wardyn system prompt is
// prepended to the user message (this is what carries the propose/clarify
// instructions to the model).
func (c *cliComposer) runCodex(ctx context.Context, schemaFile, system, user string) ([]byte, error) {
	outPath, cleanup, err := tempOutputFile("wardyn-codex-out-*.json")
	if err != nil {
		return nil, err
	}
	defer cleanup()

	prompt := user
	if strings.TrimSpace(system) != "" {
		prompt = system + "\n\n" + user
	}

	args := []string{
		"exec",
		"--output-schema", schemaFile,
		"-o", outPath,
		"--sandbox", "read-only",
		"--ask-for-approval", "never",
	}
	if c.model != "" {
		args = append(args, "-m", c.model)
	}
	args = append(args, prompt)

	if _, err := c.exec(ctx, os.Environ(), args...); err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("cli composer: read codex output file: %w", err)
	}
	// An empty/whitespace output file is a malformed attempt (the CLI ran but
	// produced no schema object): hand the bytes to ParseProposal so the canonical
	// loop retries and ultimately fails closed, rather than short-circuiting.
	return raw, nil
}

// exec runs the CLI binary with the given args and environment, returning stdout.
// A missing binary, a non-zero exit, and a timeout are each reported as a clear,
// distinct error so the caller (and the human) knows what happened.
//
// The child is started in its own process group and, on context cancel/timeout,
// the WHOLE group is signalled — otherwise a wrapper shell's grandchildren (e.g.
// a `sleep`) keep the stdout pipe open and Wait would block past the deadline.
// WaitDelay bounds that wait as a backstop.
func (c *cliComposer) exec(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.binPath, args...) //nolint:gosec // operator-configured CLI path
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	setProcGroup(cmd)
	cmd.Cancel = func() error { return killProcGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	if err == nil {
		return []byte(stdout.String()), nil
	}

	// Distinguish a timeout (context deadline) / cancel from other failures.
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return nil, fmt.Errorf("cli composer: %s timed out after %s", c.tool, c.timeout)
		}
		return nil, ctxErr
	}

	// A missing/unexecutable binary surfaces as *exec.Error or *fs.PathError
	// (depending on whether the path was resolved via PATH or given absolutely).
	var execErr *exec.Error
	if errors.As(err, &execErr) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return nil, fmt.Errorf("cli composer: %s binary %q not found or not executable: %w", c.tool, c.binPath, err)
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("cli composer: %s exited %d: %s", c.tool, exitErr.ExitCode(), msg)
	}
	return nil, fmt.Errorf("cli composer: %s invocation failed: %w", c.tool, err)
}

// writeSchemaFile writes the given JSON Schema to a temp file and returns its path
// plus a cleanup func that removes it.
func writeSchemaFile(schema map[string]any, name string) (string, func(), error) {
	body, err := json.Marshal(schema)
	if err != nil {
		return "", func() {}, fmt.Errorf("cli composer: marshal schema: %w", err)
	}
	f, err := os.CreateTemp("", "wardyn-"+name+"-*.json")
	if err != nil {
		return "", func() {}, fmt.Errorf("cli composer: create schema file: %w", err)
	}
	path := f.Name()
	cleanup := func() { os.Remove(path) }
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("cli composer: write schema file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("cli composer: close schema file: %w", err)
	}
	return path, cleanup, nil
}

// tempOutputFile creates a temp file (matching pattern) for a CLI to write its
// output to and returns its path plus a cleanup func that removes it. The file is
// created then immediately closed so the CLI subprocess can open and write it
// fresh; the caller reads it back after the subprocess exits.
func tempOutputFile(pattern string) (string, func(), error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", func() {}, fmt.Errorf("cli composer: create output file: %w", err)
	}
	path := f.Name()
	cleanup := func() { os.Remove(path) }
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("cli composer: close output file: %w", err)
	}
	return path, cleanup, nil
}

// scrubAnthropicKey returns env with ANTHROPIC_API_KEY removed so the claude CLI
// authenticates with the resident subscription session rather than an API key
// (and never bills or leaks one). It does not mutate the input slice.
func scrubAnthropicKey(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
