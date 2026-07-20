// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package sandbox implements a composer.Composer that runs the REAL `claude`
// binary INSIDE a governed one-shot Wardyn sandbox, credentialed by the
// Wardyn-managed subscription token injected PROXY-SIDE (never resident). It is
// the container-mode counterpart to the `cli` wire: the cli backend shells out to
// a claude installed on the CONTROL-PLANE host under the operator's resident
// login; a distroless container-mode wardynd has no host claude and no resident
// login, so instead it dispatches a throwaway claude-code run whose managed-
// subscription policy makes the proxy inject the live token on the wire. It is
// ToS-clean (the real claude binary under a real subscription, not header
// spoofing) and least-privilege (plan mode + the same tool denylist as the cli
// wire) — see maybe_exec_compose_mode in deploy/images/common/agent-run-lib.sh.
//
// Trust model: identical to the cli wire's, plus the sandbox confinement — the
// claude invocation runs `--permission-mode plan` with the full built-in tool
// denylist and `--strict-mcp-config`, so a prompt-injected attachment can take no
// action; its output is Grade+Clamped downstream regardless.
//
// The run-launch machinery (create + wait for a governed run, read the uploaded
// proposal) lives in the api package (it needs run creation), so the backend
// calls out through a LATE-BOUND callback the Server sets after construction —
// the registry is built at boot before the Server exists.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/composer/backends/cli"
)

// RunClaudeFunc launches ONE governed compose run: it dispatches a claude-code
// sandbox credentialed by the managed subscription (token injected proxy-side),
// materializes promptJSON into the in-sandbox claude wire, waits for the run to
// finish, and returns claude's raw stdout (the `-p --output-format json`
// wrapper). Provided by *api.Server.RunClaudeCompose and late-bound via
// SetRunClaude — nil until the Server wires it.
type RunClaudeFunc func(ctx context.Context, promptJSON []byte) (rawStdout []byte, err error)

// ComposePrompt is the wire between this backend (which assembles the
// prompt/schema with the SAME helpers the cli wire uses) and the run launcher
// (which base64-encodes each field into the sandbox env the in-sandbox claude
// wire reads). Marshaled to the promptJSON blob handed to RunClaudeFunc. Kept
// here so producer (backend) and consumer (api launcher) share one definition.
type ComposePrompt struct {
	System          string          `json:"system"`           // --append-system-prompt
	User            string          `json:"user"`             // -p
	Schema          json.RawMessage `json:"schema"`           // --json-schema (inline)
	Model           string          `json:"model,omitempty"`  // --model (empty = CLI default / operator pin)
	DisallowedTools string          `json:"disallowed_tools"` // --disallowedTools (cli.ComposerDisallowedTools)
	MaxTurns        string          `json:"max_turns"`        // --max-turns (cli.ComposerMaxTurns)
}

// Config configures the sandbox composer backend.
type Config struct {
	// Model is the model id passed to the in-sandbox claude (e.g. "opus"). Empty
	// lets the CLI use its configured default (or the operator's ANTHROPIC_MODEL
	// pin, which dispatch already sets on the sandbox env).
	Model string
}

// Composer runs the AI Run Composer's claude wire inside a governed sandbox.
type Composer struct {
	model string
	// runClaude is late-bound by the Server after construction (the launcher needs
	// run-creation machinery that does not exist when the registry is built).
	runClaude RunClaudeFunc
}

// New returns a sandbox composer backend. Its run launcher is NOT wired here —
// the Server sets it via SetRunClaude after construction (late-binding), so a
// backend whose SetRunClaude was never called fails Propose with a clear error
// rather than a nil-deref.
func New(cfg Config) *Composer {
	return &Composer{model: strings.TrimSpace(cfg.Model)}
}

// SetRunClaude late-binds the governed-run launcher. Called once by api.New after
// the Server exists (see the composeRunnerSink wiring there). Idempotent.
func (c *Composer) SetRunClaude(fn RunClaudeFunc) { c.runClaude = fn }

// Propose assembles the prompt/schema (reusing the canonical composer helpers,
// exactly as the cli wire does), runs ONE governed compose run via the late-bound
// callback, and parses claude's raw stdout through the canonical
// ProposeWithRetry loop. A single attempt: the model already ran once inside the
// sandbox, so there is nothing to re-run without launching another whole run —
// re-parsing the same bytes would be pointless. Invalid output fails closed.
func (c *Composer) Propose(ctx context.Context, req composer.ComposeRequest) (composer.Proposal, error) {
	if err := composer.ValidateRequest(req); err != nil {
		return composer.Proposal{}, err
	}
	if c.runClaude == nil {
		return composer.Proposal{}, errors.New("sandbox composer: no governed-run launcher wired (SetRunClaude not called)")
	}

	schema, err := json.Marshal(composer.ProposalJSONSchema())
	if err != nil {
		return composer.Proposal{}, fmt.Errorf("sandbox composer: marshal schema: %w", err)
	}
	promptJSON, err := json.Marshal(ComposePrompt{
		System:          composer.SystemPrompt(),
		User:            composer.BuildUserMessage(req),
		Schema:          schema,
		Model:           c.model,
		DisallowedTools: cli.ComposerDisallowedTools,
		MaxTurns:        cli.ComposerMaxTurns,
	})
	if err != nil {
		return composer.Proposal{}, fmt.Errorf("sandbox composer: marshal prompt: %w", err)
	}

	return composer.ProposeWithRetry(ctx, 1, func(ctx context.Context, _ int) ([]byte, error) {
		raw, rerr := c.runClaude(ctx, promptJSON)
		if rerr != nil {
			return nil, rerr // transport/run failure — not retried
		}
		// Parse the claude wrapper with the EXACT same extractor the cli wire uses.
		return cli.ExtractProposalJSON(raw)
	})
}
