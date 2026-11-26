// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
)

type mcpSandboxKey struct{}

// WithMCPSandbox marks a request as made by the MCP proxy of the run's server key, which the control plane
// read from the server's own row, never from the request.
func WithMCPSandbox(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, mcpSandboxKey{}, key)
}

// MCPSandboxOf is the server key WithMCPSandbox set, "" when the request is not an MCP proxy's.
func MCPSandboxOf(ctx context.Context) string {
	key, _ := ctx.Value(mcpSandboxKey{}).(string)
	return key
}

// StampMCPSandbox adds the datum mcp_sandbox, the server key, to a row an MCP proxy's request writes, over
// any value the payload carried, and leaves every other row as it is.
func StampMCPSandbox(ctx context.Context, data json.RawMessage) json.RawMessage {
	key := MCPSandboxOf(ctx)
	if key == "" {
		return data
	}
	fields := map[string]json.RawMessage{}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &fields); err != nil {
			fields = map[string]json.RawMessage{"data": data}
		}
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	fields["mcp_sandbox"], _ = json.Marshal(key)
	out, _ := json.Marshal(fields)
	return out
}
