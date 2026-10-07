// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A lost renewal reply. CreateToken rotates the refresh token, so a request AWS
// accepted whose reply never came back leaves the stored pair dead: the retry
// then meets invalid_grant and the sign-in is removed. The retry is still right
// (a throttle, or a request that never arrived, succeeds on it), so nothing here
// changes what happens. It makes it legible: the refresh failure row and the
// credential.expired.delete row carry after_lost_reply, which is consistent with
// a lost reply and never proof that AWS replaced the token.

import (
	"context"
	"errors"
	"net/http/httptrace"
	"sync/atomic"
)

// awsSSOReplyLostError is a CreateToken attempt that failed after its request
// was written in full and before any response arrived. A response of any kind,
// including an error page a proxy wrote itself, is a reply and is never this.
type awsSSOReplyLostError struct{ err error }

func (e awsSSOReplyLostError) Error() string { return e.err.Error() }
func (e awsSSOReplyLostError) Unwrap() error { return e.err }

// awsSSOReplyLost reports whether one attempt's error is a lost reply.
func awsSSOReplyLost(err error) bool {
	var lost awsSSOReplyLostError
	return errors.As(err, &lost)
}

// awsSSOSpentError is a CreateToken refusal of the spent class
// (errAWSSSOCredentialSpent, awsSSOErrorIsSpent) with the code the provider
// answered.
type awsSSOSpentError struct{ code string }

func (e awsSSOSpentError) Error() string { return errAWSSSOCredentialSpent.Error() + ": " + e.code }
func (e awsSSOSpentError) Unwrap() error { return errAWSSSOCredentialSpent }

// awsSSOEndedInvalidGrant reports whether one attempt ended invalid_grant: the
// answer a replaced refresh token gets, and so the only one after_lost_reply may
// follow. The other spent codes (an expired token, a client registration that
// is gone) are not what a lost reply leaves behind.
func awsSSOEndedInvalidGrant(err error) bool {
	var spent awsSSOSpentError
	return errors.As(err, &spent) && spent.code == "invalid_grant"
}

// awsSSORefreshFailureData is a failed redemption's harness.credential.refresh
// row data; after_lost_reply rides it only when true.
func awsSSORefreshFailureData(spent bool, err error, attempts int, afterLostReply bool) map[string]any {
	data := map[string]any{"provider": awsSSOProvider, "spent": spent, "error": err.Error(), "attempts": attempts}
	if afterLostReply {
		data["after_lost_reply"] = true
	}
	return data
}

// traceRequestWritten returns ctx with a trace that notes the request was
// written in full, and the reader of that note. WroteRequest is also called on a
// failed write, with Err set: AWS may never have seen that request, so it does
// not count.
func traceRequestWritten(ctx context.Context) (context.Context, func() bool) {
	var wrote atomic.Bool
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
	}), wrote.Load
}
