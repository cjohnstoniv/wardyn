// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package scim

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
)

// scimType values of the RFC 7644 error envelope (section 3.12) this package produces.
const (
	TypeInvalidFilter = "invalidFilter"
	TypeInvalidSyntax = "invalidSyntax"
	TypeInvalidPath   = "invalidPath"
	TypeInvalidValue  = "invalidValue"
	TypeNoTarget      = "noTarget"
)

// Error is the RFC 7644 error envelope. Status is the HTTP status the handler answers with; the wire form
// carries it as a string, as the RFC's examples do.
type Error struct {
	Status   int
	ScimType string
	Detail   string
}

// NewError builds an envelope for status and scimType (empty for the types the RFC leaves untyped, such as 404).
func NewError(status int, scimType, detail string) *Error {
	return &Error{Status: status, ScimType: scimType, Detail: detail}
}

func badRequest(scimType, detail string) *Error { return NewError(400, scimType, detail) }

func (e *Error) Error() string {
	if e.ScimType == "" {
		return strconv.Itoa(e.Status) + ": " + e.Detail
	}
	return strconv.Itoa(e.Status) + " " + e.ScimType + ": " + e.Detail
}

// MarshalJSON renders the wire envelope.
func (e *Error) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Schemas  []string `json:"schemas"`
		ScimType string   `json:"scimType,omitempty"`
		Detail   string   `json:"detail"`
		Status   string   `json:"status"`
	}{[]string{SchemaError}, e.ScimType, e.Detail, strconv.Itoa(e.Status)})
}

// decodeOne decodes exactly one JSON value into v and refuses trailing data. A failure is invalidSyntax.
func decodeOne(body []byte, v any) *Error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(v); err != nil {
		return badRequest(TypeInvalidSyntax, "request body is not valid JSON for this resource")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return badRequest(TypeInvalidSyntax, "request body has data after the JSON value")
	}
	return nil
}
