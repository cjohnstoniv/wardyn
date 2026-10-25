// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"errors"
	"fmt"
	"strings"
)

// proxyExitLapsed is wardyn-proxy's exit status once it lapses (exitLapsed in cmd/wardyn-proxy): a
// runner's proxy that could not renew its run token, by a permanent refusal or by failing until its
// first renew's bound. Its termination message is then its last renew error, already redacted:
// it writes that to the default terminationMessagePath, which FallbackToLogsOnError reads before
// the log tail.
const proxyExitLapsed = 3

// maxProxyRenewCause caps the cause read back; an older proxy's message is its log tail.
const maxProxyRenewCause = 512

// proxyRenewFailure names a lapsed proxy's start failure by its last renew error.
func proxyRenewFailure(cause string) error {
	const failed = "the run's proxy could not renew its run token with the organisation"
	cause = strings.Join(strings.Fields(cause), " ")
	if len(cause) > maxProxyRenewCause {
		cause = strings.ToValidUTF8(cause[:maxProxyRenewCause], "") + "…"
	}
	if cause == "" {
		return errors.New(failed)
	}
	return fmt.Errorf("%s: %s", failed, cause)
}
