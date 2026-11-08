// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// ModelUsageTokens are token counts exactly as the provider reported them. Anthropic and Bedrock count input
// without the cache tokens; OpenAI counts cached tokens inside input. The fields are kept apart and never
// added together. Calls counts every call recorded; UncountedCalls and PartialCalls are subsets of it.
type ModelUsageTokens struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	Calls            int64 `json:"calls"`
	UncountedCalls   int64 `json:"uncounted_calls"`
	PartialCalls     int64 `json:"partial_calls"`
}

// ModelUsageModel is one model's line: the provider row its host belongs to (the host itself when none
// matches), the vendor dialect its counts are in, and why its uncounted calls were not counted.
type ModelUsageModel struct {
	Provider         string           `json:"provider"`
	Vendor           string           `json:"vendor"`
	Host             string           `json:"host"`
	Model            string           `json:"model"`
	UncountedReasons map[string]int64 `json:"uncounted_reasons"`
	ModelUsageTokens
}

// ModelUsage is a set of runs' usage: totals, one line per model, and the tunnels to model hosts Wardyn could
// not open (tunnels, not calls).
type ModelUsage struct {
	Totals        ModelUsageTokens  `json:"totals"`
	ByModel       []ModelUsageModel `json:"by_model"`
	OpaqueTunnels int64             `json:"opaque_tunnels"`
	UpdatedAt     *time.Time        `json:"updated_at,omitempty"`
}

// UnitModelUsageLine is the usage of the runs frozen to one unit.
type UnitModelUsageLine struct {
	UnitID   uuid.UUID `json:"unit_id"`
	UnitName string    `json:"unit_name"`
	ModelUsage
}

// UnitModelUsage is a unit's token usage over a window: the unit asked about first, then each unit below it
// that has usage. Tokens are shown, not cost.
type UnitModelUsage struct {
	Since time.Time            `json:"since"`
	Until time.Time            `json:"until"`
	Units []UnitModelUsageLine `json:"units"`
}

// UnitModelUsage reads the token usage of the runs frozen to unitID and the units below it, created in
// [since, until). GET /api/v1/governance/units/{id}/model-usage, for a unit admin of the unit or a unit above
// it, or a security admin. A zero time sends no parameter: until defaults to now and since to 30 days before
// it, and the server refuses a window of more than 92 days with 400 model_usage_window_invalid.
func (c *Client) UnitModelUsage(ctx context.Context, unitID uuid.UUID, since, until time.Time) (UnitModelUsage, error) {
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	if !until.IsZero() {
		q.Set("until", until.UTC().Format(time.RFC3339))
	}
	path := "/api/v1/governance/units/" + unitID.String() + "/model-usage"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out UnitModelUsage
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}
