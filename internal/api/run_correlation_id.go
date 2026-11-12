// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
)

// maxCorrelationIDLen bounds the caller's reference: it rides in every audit row of the run that reaches a sink.
const maxCorrelationIDLen = 128

const correlationIDInvalidMsg = "The correlation ID can hold at most 128 characters: ASCII letters, digits and . _ : / -."

// correlationID is the request's reference as the run keeps it: trimmed, "" when none was given.
func correlationID(raw string) string { return strings.TrimSpace(raw) }

// correlationIDRefusal checks the trimmed correlation_id against the bounded charset. The charset is closed
// on purpose: the value reaches logs and collectors, so nothing that could inject a line or a control byte
// gets in. It is never a metric label.
func correlationIDRefusal(req createRunRequest) *runRefusal {
	id := correlationID(req.CorrelationID)
	if len(id) > maxCorrelationIDLen || strings.ContainsFunc(id, func(r rune) bool { return !correlationIDRune(r) }) {
		return runError(http.StatusUnprocessableEntity, reasonCorrelationIDInvalid, correlationIDInvalidMsg)
	}
	return nil
}

func correlationIDRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:/-", r)
}
