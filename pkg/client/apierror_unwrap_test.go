// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"testing"

	client "github.com/cjohnstoniv/wardyn/pkg/client"
)

// These pin the server-envelope unwrap in APIError.Error(). An Error() that
// printed the raw body fails these (the message would carry the literal JSON
// `{`) — a user-visible break in error unwrapping.

func TestAPIError_UnwrapsErrorField(t *testing.T) {
	e := &client.APIError{Status: 400, Body: `{"error":"invalid state filter"}`}
	if got, want := e.Error(), "API error 400: invalid state filter"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAPIError_UnwrapsMessageField(t *testing.T) {
	e := &client.APIError{Status: 409, Body: `{"message":"already decided"}`}
	if got, want := e.Error(), "API error 409: already decided"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAPIError_FallsBackToRawBodyForNonEnvelope(t *testing.T) {
	e := &client.APIError{Status: 502, Body: "upstream boom"}
	if got, want := e.Error(), "API error 502: upstream boom"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
