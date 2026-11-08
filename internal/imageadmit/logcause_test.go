// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	if got := LogRegistry(&Unavailable{Cause: CauseTLS, Host: "realm.example:443", Registry: "r.example", Err: errors.New(secret)}); got != "r.example" {
		t.Errorf("LogRegistry = %q, want the image's registry, not the realm", got)
	}
	if got := LogRegistry(errors.New(secret)); got != "" {
		t.Errorf("LogRegistry of a plain error = %q, want empty", got)
	}
	for _, c := range cases {
		got := LogCause(c.err)
		if got != c.want || strings.Contains(got, secret) {
			t.Errorf("LogCause(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestParseManifest_AnnouncedDigestNotEchoed: a Docker-Content-Digest that does not match is refused
// without repeating its value, which a registry sent a Basic credential could set to that credential.
func TestParseManifest_AnnouncedDigestNotEchoed(t *testing.T) {
	const echoed = "sha256:hunter2-registry-password"
	hdr := http.Header{}
	hdr.Set("Docker-Content-Digest", echoed)
	_, err := parseManifest([]byte(`{}`), hdr, "")
	if !errors.Is(err, ErrDigestMismatch) || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("err = %v, want a digest mismatch that does not repeat the announced value", err)
	}
}
