// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestLogCause_FixedWordsOnly: the log word for a failed check is one of a fixed set, whatever text the
// error wraps, so a credential or a registry's answer inside an error never reaches the log.
func TestLogCause_FixedWordsOnly(t *testing.T) {
	const secret = "hunter2-registry-password"
	cases := []struct {
		err  error
		want string
	}{
		{&Unavailable{Cause: CauseAuthFile, Host: "r.example", Err: errors.New(secret)}, "auth_file"},
		{&Unavailable{Cause: CauseRefused, Host: "r.example", Err: errors.New(secret)}, "refused"},
		{&Unavailable{Cause: CauseUnauthorized, Host: "r.example"}, "unauthorized"},
		{&Unavailable{Cause: Cause("new"), Host: "r.example", Err: errors.New(secret)}, "unavailable"},
		{fmt.Errorf("wrapped %s: %w", secret, context.DeadlineExceeded), "timeout"},
		{fmt.Errorf("%s: %w", secret, ErrNotFound), "not_found"},
		{errors.New(secret), "other"},
	}
	for _, c := range cases {
		got := LogCause(c.err)
		if got != c.want || strings.Contains(got, secret) {
			t.Errorf("LogCause(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
