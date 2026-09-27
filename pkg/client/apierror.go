// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"fmt"
)

// APIError is returned when the server responds with a non-2xx status code.
// Status is the HTTP status code; Body is the raw response body (trimmed to
// 2 KiB) for diagnostic display. Callers may use errors.As to extract it.
type APIError struct {
	// Status is the HTTP status code, e.g. 404.
	Status int
	// Body is the raw server response body (capped at 2048 bytes).
	Body string
	// Reason is the server's machine-readable refusal class (internal/api's
	// errorBody.Reason), "" when the route does not send one — most do not yet
	// (#204 is phasing coverage in lane by lane). A caller that needs to branch
	// on WHY a call failed reads Reason, never Error()'s prose: the human
	// sentence is free to reword without notice, the reason string is not.
	Reason string
}

// newAPIError builds an *APIError from a non-2xx status and its raw body,
// parsing the server's {"error", "reason"} envelope once so every call site
// need not repeat it.
func newAPIError(status int, raw []byte) *APIError {
	e := &APIError{Status: status, Body: string(raw)}
	var env struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(raw, &env) == nil {
		e.Reason = env.Reason
	}
	return e
}

func (e *APIError) Error() string {
	if msg := e.envelopeMessage(); msg != "" {
		return fmt.Sprintf("API error %d: %s", e.Status, msg)
	}
	return fmt.Sprintf("API error %d: %s", e.Status, e.Body)
}

// envelopeMessage extracts the human-readable message from the server's
// standard {"error":...} (or {"message":...}) JSON envelope, returning "" when
// Body is not such an envelope so Error() falls back to the raw body. Without
// it a failed call surfaces raw JSON to the caller (e.g. the CLI) instead of
// the message — the regression the CLI's old transport avoided by unwrapping.
func (e *APIError) envelopeMessage() string {
	var env struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(e.Body), &env) != nil {
		return ""
	}
	if env.Error != "" {
		return env.Error
	}
	return env.Message
}
