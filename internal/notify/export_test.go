// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"net/http"
	"time"
)

// SendMail exposes the SMTP transport to the external test package: it returns the failure class (empty
// on success) and whether the worker would retry.
func SendMail(ctx context.Context, ch Channel, client *http.Client, body []byte, now time.Time) (string, bool) {
	r := ch.sendMail(ctx, client, body, now)
	return r.class, r.retryable
}
