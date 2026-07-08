// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package openai implements the Wardyn Run Composer backend that talks to
// OpenAI-wire Chat Completions APIs. The same wire (and request shaping) covers
// three transports that differ ONLY by base URL + auth:
//
//   - "api":        api.openai.com (option.WithAPIKey, optional WithBaseURL).
//   - "compatible": any OpenAI-compatible BYOK/BYOM endpoint (Ollama, vLLM,
//     LiteLLM, OpenRouter, …) reached via WithBaseURL + WithAPIKey (the key may
//     be a placeholder for keyless local servers).
//   - "azure":      Azure OpenAI's v1 surface (cfg.BaseURL + "/openai/v1"),
//     authenticated with an Api-Key header ("apikey") or a Microsoft Entra
//     bearer token from DefaultAzureCredential ("entra"); the model is the Azure
//     DEPLOYMENT name.
//
// In every case the request forces strict structured output via
// response_format=json_schema (name = composer.ProposalSchemaName, strict=true,
// schema = composer.ProposalJSONSchema()), the system message is
// composer.SystemPrompt(), and the user message is composer.BuildUserMessage().
// The assistant message content (a single JSON object) is fed to
// composer.ProposeWithRetry, which parses, validates, and bounds retries —
// failing closed. An OpenAI refusal is treated as a retryable invalid output.
package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/composer/backends/transport"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// Transport selects which OpenAI-wire endpoint + auth the backend uses.
const (
	TransportAPI        = "api"
	TransportAzure      = "azure"
	TransportCompatible = "compatible"
)

// Azure auth modes.
const (
	AzureAuthAPIKey = "apikey"
	AzureAuthEntra  = "entra"
)

// openaiProductionBaseURL is the canonical api.openai.com endpoint. We pin it
// explicitly for the "api" transport when the caller gives no BaseURL so an
// ambient OPENAI_BASE_URL in the daemon environment cannot redirect the request.
const openaiProductionBaseURL = "https://api.openai.com/v1/"

// Config configures an OpenAI-wire composer backend. Transport picks the
// endpoint family; the remaining fields are interpreted per-transport (see the
// package doc and NewComposer).
type Config struct {
	// Transport is "api", "azure", or "compatible".
	Transport string
	// Model is the model id ("api"/"compatible") or the Azure DEPLOYMENT name
	// ("azure").
	Model string
	// APIKey is the bearer key ("api"), the (possibly dummy) key for a
	// "compatible" server, or the Api-Key for "azure" apikey auth.
	APIKey string
	// BaseURL overrides the endpoint. Optional for "api"; REQUIRED for
	// "compatible" and "azure" (for azure it is the resource root, e.g.
	// https://<res>.openai.azure.com — "/openai/v1" is appended).
	BaseURL string
	// AzureAuth is "apikey" or "entra" (azure transport only; defaults to
	// "apikey").
	AzureAuth string
	// MaxAttempts bounds the parse-validate-retry loop (<=0 → package default).
	MaxAttempts int

	// httpClient overrides the hardened LLM-egress client. Unexported: production
	// always gets the governed client built in NewComposer; tests set this to an
	// httptest server's client so they can exercise the wire over loopback.
	httpClient *http.Client
}

// backend is the constructed Composer for an OpenAI-wire transport.
type backend struct {
	client      oai.Client
	model       shared.ChatModel
	maxAttempts int
}

// NewComposer validates cfg and builds an OpenAI-wire Composer. It never
// performs network I/O: for "entra" it only constructs the credential.
func NewComposer(cfg Config) (composer.Composer, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("openai: Model is required")
	}

	var opts []option.RequestOption
	// allowHost / allowPrivate parameterize the hardened LLM-egress client built
	// after the switch: allowHost is the single endpoint host this backend may
	// reach; allowPrivate relaxes the loopback/RFC1918 SSRF block ONLY for the
	// "compatible" transport, whose documented use includes a local BYOM server
	// (Ollama / vLLM on localhost). The cloud-metadata range stays blocked even
	// then.
	var allowHost string
	var allowPrivate bool

	switch cfg.Transport {
	case TransportAPI:
		if strings.TrimSpace(cfg.APIKey) == "" {
			return nil, errors.New("openai: APIKey is required for the \"api\" transport")
		}
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
		// Always pin the base URL. If the caller did not override it we pin the
		// canonical production endpoint, so an ambient OPENAI_BASE_URL in the
		// daemon environment can NOT silently redirect the request (and the
		// configured key) to an operator-unintended host.
		base := cfg.BaseURL
		if strings.TrimSpace(base) == "" {
			base = openaiProductionBaseURL
		}
		opts = append(opts, option.WithBaseURL(base))
		allowHost = transport.HostOf(base)

	case TransportCompatible:
		// BYOK/BYOM: the endpoint is mandatory; the key may be a placeholder for
		// a keyless local server, but the SDK requires a non-empty Authorization
		// value, so reject a truly empty key (callers pass a dummy if needed).
		if strings.TrimSpace(cfg.BaseURL) == "" {
			return nil, errors.New("openai: BaseURL is required for the \"compatible\" transport")
		}
		if strings.TrimSpace(cfg.APIKey) == "" {
			return nil, errors.New("openai: APIKey is required for the \"compatible\" transport (use a dummy value for keyless servers)")
		}
		opts = append(opts,
			option.WithBaseURL(cfg.BaseURL),
			option.WithAPIKey(cfg.APIKey),
		)
		allowHost = transport.HostOf(cfg.BaseURL)
		allowPrivate = true // operator-configured local BYOM endpoint may be on loopback

	case TransportAzure:
		if strings.TrimSpace(cfg.BaseURL) == "" {
			return nil, errors.New("openai: BaseURL is required for the \"azure\" transport")
		}
		// Use the Azure OpenAI v1 surface; the model field is the deployment
		// name and is sent in the request body (no legacy path rewrite needed).
		azureBase := azureV1BaseURL(cfg.BaseURL)
		opts = append(opts, option.WithBaseURL(azureBase))
		allowHost = transport.HostOf(azureBase)
		// Azure authenticates via the Api-Key header or an Entra bearer
		// middleware — never the SDK's own Bearer key. Clear any ambient
		// OPENAI_API_KEY the SDK picked up from the environment so it cannot
		// leak as a stray Authorization: Bearer header to the Azure resource.
		opts = append(opts, option.WithAPIKey(""))

		auth := cfg.AzureAuth
		if auth == "" {
			auth = AzureAuthAPIKey
		}
		switch auth {
		case AzureAuthAPIKey:
			if strings.TrimSpace(cfg.APIKey) == "" {
				return nil, errors.New("openai: APIKey is required for azure apikey auth")
			}
			opts = append(opts, azure.WithAPIKey(cfg.APIKey))
		case AzureAuthEntra:
			cred, err := azidentity.NewDefaultAzureCredential(nil)
			if err != nil {
				return nil, fmt.Errorf("openai: azure entra credential: %w", err)
			}
			opts = append(opts, azure.WithTokenCredential(cred))
		default:
			return nil, fmt.Errorf("openai: unknown AzureAuth %q (want %q or %q)", cfg.AzureAuth, AzureAuthAPIKey, AzureAuthEntra)
		}

	default:
		return nil, fmt.Errorf("openai: unknown Transport %q (want %q, %q, or %q)", cfg.Transport, TransportAPI, TransportAzure, TransportCompatible)
	}

	// Pin a hardened HTTP client for the (governed) LLM egress: it reaches ONLY
	// allowHost, ignores any ambient HTTP(S)_PROXY, refuses redirects to off-list
	// hosts, and refuses any host resolving to a private/loopback/metadata IP
	// (SSRF guard). Appended AFTER the auth/base-URL options so it is in force;
	// the test seam below overrides it last.
	opts = append(opts, option.WithHTTPClient(transport.NewClient([]string{allowHost}, allowPrivate, 0)))
	if cfg.httpClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.httpClient))
	}

	return &backend{
		client:      oai.NewClient(opts...),
		model:       shared.ChatModel(cfg.Model),
		maxAttempts: cfg.MaxAttempts,
	}, nil
}

// azureV1BaseURL appends the "/openai/v1" path to the Azure resource root,
// trimming any trailing slash so the SDK joins the path cleanly.
func azureV1BaseURL(root string) string {
	return strings.TrimRight(root, "/") + "/openai/v1"
}

// Propose shapes ONE Chat Completions request per attempt and delegates the
// parse-validate-bounded-retry loop (and fail-closed) to the foundation.
func (b *backend) Propose(ctx context.Context, req composer.ComposeRequest) (composer.Proposal, error) {
	params := b.buildParams(req, composer.SystemPrompt(), composer.ProposalJSONSchema(), composer.ProposalSchemaName)
	return composer.ProposeWithRetry(ctx, b.maxAttempts, func(ctx context.Context, _ int) ([]byte, error) {
		return b.issue(ctx, params)
	})
}

// Clarify runs the SAME Chat Completions wire with the clarify schema + prompt.
func (b *backend) Clarify(ctx context.Context, req composer.ComposeRequest) (composer.Clarification, error) {
	params := b.buildParams(req, composer.ClarifySystemPrompt(), composer.ClarificationJSONSchema(), composer.ClarifySchemaName)
	return composer.ClarifyWithRetry(ctx, b.maxAttempts, func(ctx context.Context, _ int) ([]byte, error) {
		return b.issue(ctx, params)
	})
}

// buildParams shapes the Chat Completions request forcing the given schema/name
// via response_format=json_schema (strict). The system prompt + schema are
// parameters so the same wire drives both the propose and clarify steps.
func (b *backend) buildParams(req composer.ComposeRequest, system string, schema map[string]any, schemaName string) oai.ChatCompletionNewParams {
	return oai.ChatCompletionNewParams{
		Model: b.model,
		Messages: []oai.ChatCompletionMessageParamUnion{
			oai.SystemMessage(system),
			oai.UserMessage(composer.BuildUserMessage(req)),
		},
		ResponseFormat: oai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   schemaName,
					Strict: oai.Bool(true),
					Schema: schema,
				},
			},
		},
	}
}

// Assist answers ONE plain-language operator question about the proposed setup as
// INERT advisory text. It reuses the SAME hardened client + single-shot Chat
// Completions wire, but with NO ResponseFormat (no json_schema) so the model
// replies in free prose. The answer is capped and never re-graded/clamped.
func (b *backend) Assist(ctx context.Context, req composer.ComposeRequest, question string) (string, error) {
	params := oai.ChatCompletionNewParams{
		Model:               b.model,
		MaxCompletionTokens: oai.Int(composer.AssistMaxTokens),
		Messages: []oai.ChatCompletionMessageParamUnion{
			oai.SystemMessage(composer.AssistSystemPrompt),
			oai.UserMessage(composer.AssistUserMessage(req, question)),
		},
	}
	resp, err := b.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("openai: assist request: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("openai: assist response had no choices")
	}
	msg := resp.Choices[0].Message
	if strings.TrimSpace(msg.Refusal) != "" {
		return "", fmt.Errorf("openai: assist refused: %s", strings.TrimSpace(msg.Refusal))
	}
	out := strings.TrimSpace(msg.Content)
	if out == "" {
		return "", errors.New("openai: assist response had empty content")
	}
	return out, nil
}

// issue performs ONE Chat Completions request and returns the raw JSON bytes. A
// refusal / empty / missing message is handed back as non-JSON bytes (NOT a
// transport error) so the foundation treats it as RETRYABLE invalid output and a
// persistently-refusing model fails closed after maxAttempts.
func (b *backend) issue(ctx context.Context, params oai.ChatCompletionNewParams) ([]byte, error) {
	resp, err := b.client.Chat.Completions.New(ctx, params)
	if err != nil {
		// Genuine transport/backend failure: return an error so the caller maps it
		// to a 502 (the SDK owns its own HTTP retries; the foundation does not
		// retry these).
		return nil, fmt.Errorf("openai: chat completion request: %w", err)
	}
	if len(resp.Choices) == 0 {
		return []byte("openai: response contained no choices"), nil
	}
	msg := resp.Choices[0].Message
	if strings.TrimSpace(msg.Refusal) != "" {
		return []byte("openai: model refused: " + msg.Refusal), nil
	}
	if strings.TrimSpace(msg.Content) == "" {
		return []byte("openai: response message had empty content"), nil
	}
	return []byte(msg.Content), nil
}
