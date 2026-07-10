// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package anthropic implements the composer.Composer backend that drives
// Anthropic's Messages API (first-party API or Amazon Bedrock) to produce a Wardyn
// run proposal.
//
// It forces the model to emit ONLY the portable strict JSON Schema from package
// composer, using Structured Outputs: output_config.format is set to a json_schema
// wrapping composer.ProposalJSONSchema(), and the model returns the JSON object as
// an ordinary text content block.
//
// The extracted JSON bytes are handed to composer.ProposeWithRetry, which parses,
// validates, bounds retries, and fails closed. The system prompt and user-message
// assembly (including untrusted-content fencing) come from package composer so
// every backend shares the exact same trust boundary.
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/bedrock"
	"github.com/anthropics/anthropic-sdk-go/option"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/composer/backends/transport"
)

// Transport selects how the SDK reaches Anthropic.
const (
	TransportAPI     = "api"     // first-party api.anthropic.com (API key auth)
	TransportBedrock = "bedrock" // Amazon Bedrock (AWS SigV4 / default credential chain)
)

// anthropicAPIHost is the only host the "api" transport is permitted to reach.
const anthropicAPIHost = "api.anthropic.com"

// defaultMaxTokens bounds a single proposal generation. A run proposal is a small
// JSON object; this is generous headroom, not a target.
const defaultMaxTokens = 4096

// Config configures the Anthropic composer backend. It carries NO secrets beyond
// APIKey (used only for the "api" transport); Bedrock auth flows through the AWS
// default credential chain, so only Region is needed here.
type Config struct {
	// Transport is "api" (default) or "bedrock".
	Transport string
	// Model is the model id (e.g. "claude-sonnet-4-5"); for Bedrock pass the
	// Anthropic-prefixed inference id. Required.
	Model string
	// APIKey authenticates the "api" transport. Required for "api"; ignored for
	// "bedrock".
	APIKey string
	// Region is the AWS region for the "bedrock" transport. Required for
	// "bedrock"; ignored for "api".
	Region string
	// MaxAttempts bounds composer.ProposeWithRetry's parse/validate retry loop.
	// Zero falls back to composer.DefaultMaxAttempts.
	MaxAttempts int

	// extraOptions are appended to the SDK client options (after auth). Tests use
	// it to inject option.WithBaseURL(httptest.Server.URL); production leaves it
	// empty.
	extraOptions []option.RequestOption
}

// composerImpl is the constructed backend. The SDK client is built ONCE at
// construction so Propose is cheap and free of per-call setup.
type composerImpl struct {
	client      sdk.Client
	model       sdk.Model
	maxAttempts int
}

// NewComposer validates cfg, constructs the SDK client for the chosen transport,
// and returns a composer.Composer. Validation is fail-closed: an "api" backend
// without an APIKey, or a "bedrock" backend without a Region, is an error.
func NewComposer(cfg Config) (composer.Composer, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("anthropic: Model is required")
	}
	mode := cfg.Transport
	if mode == "" {
		mode = TransportAPI
	}

	var client sdk.Client
	switch mode {
	case TransportAPI:
		if strings.TrimSpace(cfg.APIKey) == "" {
			return nil, errors.New("anthropic: api transport requires APIKey")
		}
		opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
		// Pin a hardened HTTP client: this control-plane LLM call must reach ONLY
		// api.anthropic.com, must not be redirected by an ambient HTTP(S)_PROXY,
		// and must refuse any host that resolves to a private/loopback/metadata IP
		// (SSRF guard). extraOptions are appended AFTER so a test can override the
		// client + base URL to point at an httptest server.
		opts = append(opts, option.WithHTTPClient(transport.NewClient([]string{anthropicAPIHost}, false, 0)))
		opts = append(opts, cfg.extraOptions...)
		client = sdk.NewClient(opts...)
	case TransportBedrock:
		if strings.TrimSpace(cfg.Region) == "" {
			return nil, errors.New("anthropic: bedrock transport requires Region")
		}
		// Bedrock auth + request rewriting is installed as a request option on a
		// normal Anthropic client. WithLoadDefaultConfig resolves the AWS default
		// credential chain (SigV4); no AWS creds are required at construction
		// time, so this stays hermetic until an actual request is made.
		bedrockOpt := bedrock.WithLoadDefaultConfig(context.Background(), awsconfig.WithRegion(cfg.Region))
		opts := []option.RequestOption{bedrockOpt}
		// Same hardened client, allowlisting only the regional Bedrock runtime
		// endpoint the SDK targets. (AWS credential resolution via IMDS uses the
		// AWS SDK's own client, not this one, so blocking 169.254/16 here does not
		// affect instance-role auth.)
		bedrockHost := fmt.Sprintf("bedrock-runtime.%s.amazonaws.com", strings.TrimSpace(cfg.Region))
		opts = append(opts, option.WithHTTPClient(transport.NewClient([]string{bedrockHost}, false, 0)))
		opts = append(opts, cfg.extraOptions...)
		client = sdk.NewClient(opts...)
	default:
		return nil, fmt.Errorf("anthropic: unknown transport %q (want %q or %q)", mode, TransportAPI, TransportBedrock)
	}

	return &composerImpl{
		client:      client,
		model:       cfg.Model,
		maxAttempts: cfg.MaxAttempts,
	}, nil
}

// Propose builds the request once and delegates the parse/validate/retry loop to
// composer.ProposeWithRetry; each retry re-issues the Messages request and returns
// the raw structured-output JSON bytes.
func (c *composerImpl) Propose(ctx context.Context, req composer.ComposeRequest) (composer.Proposal, error) {
	params := c.buildParams(req, composer.SystemPrompt(), composer.ProposalJSONSchema(), composer.ProposalSchemaName)
	return composer.ProposeWithRetry(ctx, c.maxAttempts, func(ctx context.Context, _ int) ([]byte, error) {
		return c.issue(ctx, params)
	})
}

// Clarify runs the SAME Messages wire with the clarify schema + prompt; the
// foundation parses each attempt with ParseClarification and fails closed.
func (c *composerImpl) Clarify(ctx context.Context, req composer.ComposeRequest) (composer.Clarification, error) {
	params := c.buildParams(req, composer.ClarifySystemPrompt(), composer.ClarificationJSONSchema(), composer.ClarifySchemaName)
	return composer.ClarifyWithRetry(ctx, c.maxAttempts, func(ctx context.Context, _ int) ([]byte, error) {
		return c.issue(ctx, params)
	})
}

// Assist answers ONE plain-language operator question about the proposed setup as
// INERT advisory text. It reuses the SAME hardened SDK client + single-shot Messages
// wire, but with NO structured output — no OutputConfig, no Tools — so the model
// replies in free prose. The answer is capped and never re-graded/clamped.
func (c *composerImpl) Assist(ctx context.Context, req composer.ComposeRequest, question string) (string, error) {
	params := sdk.MessageNewParams{
		Model:     c.model,
		MaxTokens: composer.AssistMaxTokens,
		System:    []sdk.TextBlockParam{{Text: composer.AssistSystemPrompt}},
		Messages: []sdk.MessageParam{
			sdk.NewUserMessage(sdk.NewTextBlock(composer.AssistUserMessage(req, question))),
		},
	}
	msg, err := c.client.Messages.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("anthropic: assist request failed: %w", err)
	}
	if msg == nil {
		return "", errors.New("anthropic: nil assist response")
	}
	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", errors.New("anthropic: assist response had no text content")
	}
	return out, nil
}

// issue performs ONE Messages request and returns the raw structured-output JSON.
func (c *composerImpl) issue(ctx context.Context, params sdk.MessageNewParams) ([]byte, error) {
	msg, err := c.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("anthropic: messages request failed: %w", err)
	}
	return extractJSON(msg)
}

// buildParams assembles the single MessageNewParams shared by all attempts: the
// given system prompt, fenced user message, model, max tokens, and Structured
// Outputs forcing the given schema/name. The schema/prompt are parameters so the
// same wire drives both the propose and clarify steps.
func (c *composerImpl) buildParams(req composer.ComposeRequest, system string, schema map[string]any, schemaName string) sdk.MessageNewParams {
	params := sdk.MessageNewParams{
		Model:     c.model,
		MaxTokens: defaultMaxTokens,
		System:    []sdk.TextBlockParam{{Text: system}},
		Messages: []sdk.MessageParam{
			sdk.NewUserMessage(sdk.NewTextBlock(composer.BuildUserMessage(req))),
		},
	}
	params.OutputConfig = sdk.OutputConfigParam{
		Format: sdk.JSONOutputFormatParam{Schema: schema},
	}
	return params
}

// extractJSON pulls the proposal JSON bytes out of the Messages response by
// concatenating the assistant text blocks (Structured Outputs returns the object
// as text). It fails closed when no usable content is present.
func extractJSON(msg *sdk.Message) ([]byte, error) {
	if msg == nil {
		return nil, errors.New("anthropic: nil message response")
	}
	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return nil, errors.New("anthropic: response contained no text content")
	}
	// Guard against a model that wraps the object in a markdown fence despite the
	// schema directive; ParseProposal would otherwise reject valid JSON.
	return []byte(stripCodeFence(out)), nil
}

// stripCodeFence removes a single surrounding ```/```json fence if present,
// returning the inner content; otherwise it returns s unchanged.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	rest := strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:] // drop the ```json language tag line
	}
	if j := strings.LastIndex(rest, "```"); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}
