// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// cliSignInFlags are CLI sign-in's settings (docs/ENV.md), off by default; see api.CLISignInConfig. A
// bootFlags built without registering them, as a test does, leaves the pointers nil, which reads as off.
type cliSignInFlags struct {
	enabled, paste, fileStore *bool
	ttl, idle                 *time.Duration
}

func registerCLISignInFlags() cliSignInFlags {
	return cliSignInFlags{
		enabled:   flagBool("cli-sign-in", "WARDYN_CLI_SIGN_IN", false, "let wardyn login sign a CLI in through the console, which needs SSO (default false)"),
		paste:     flagBool("cli-paste-sign-in", "WARDYN_CLI_PASTE_SIGN_IN", false, "let wardyn login --no-browser sign in by pasting a code from the console, on a computer with no browser (default false)"),
		fileStore: flagBool("cli-file-store", "WARDYN_CLI_FILE_STORE", true, "let a computer with no keychain keep its CLI sign-in in a file readable only by its owner, capped at 7 days; off refuses it (default true)"),
		ttl:       flagDuration("cli-session-ttl", "WARDYN_CLI_SESSION_TTL", 0, "how long a CLI sign-in lasts from approval (duration; 0 = 30 days; 1h to 90 days)"),
		idle:      flagDuration("cli-session-idle", "WARDYN_CLI_SESSION_IDLE", 0, "end a CLI sign-in that has not renewed for this long (duration; 0 = 7 days; at most the TTL)"),
	}
}

// validate refuses a TTL outside 1h..90 days and an idle lifetime that is negative or longer than the
// TTL in effect.
func (c cliSignInFlags) validate() error {
	ttl, idle := orZero(c.ttl), orZero(c.idle)
	if ttl != 0 && (ttl < api.MinCLISessionTTL || ttl > api.MaxCLISessionTTL) {
		return fmt.Errorf("refusing to start: WARDYN_CLI_SESSION_TTL is %s; it must be 0 (30 days) or between 1h and 2160h (90 days)", ttl)
	}
	effective := ttl
	if effective == 0 {
		effective = api.DefaultCLISessionTTL
	}
	if idle < 0 || idle > effective {
		return fmt.Errorf("refusing to start: WARDYN_CLI_SESSION_IDLE is %s; it must be 0 (7 days) or positive and at most WARDYN_CLI_SESSION_TTL (%s)", idle, effective)
	}
	return nil
}

// config is the api.CLISignInConfig these flags set. The proof origin is the SSO redirect URL without
// its /auth/callback: the scheme, host and base path the CLI signed in at.
func (c cliSignInFlags) config(oidcRedirectURL string) api.CLISignInConfig {
	origin := ""
	if u, err := url.Parse(oidcRedirectURL); err == nil && u.Host != "" {
		origin = u.Scheme + "://" + u.Host + strings.TrimSuffix(strings.TrimSuffix(u.Path, "/auth/callback"), "/")
	}
	return api.CLISignInConfig{Enabled: orZero(c.enabled), Paste: orZero(c.paste),
		FileStoreOff: c.fileStore != nil && !*c.fileStore, SessionTTL: orZero(c.ttl), SessionIdle: orZero(c.idle), Origin: origin}
}

// warnWithoutSSO says so once at boot when WARDYN_CLI_SIGN_IN is on but SSO is not configured: CLI
// sign-in needs a browser session to approve with, so it stays off (every route a 404, /healthz false).
func (c cliSignInFlags) warnWithoutSSO(oidcConfigured bool) {
	if orZero(c.enabled) && !oidcConfigured {
		slog.Warn("wardynd: WARDYN_CLI_SIGN_IN is set but SSO is not configured, so CLI sign-in stays off and the CLI keeps using WARDYN_TOKEN. Configure OIDC sign-in to turn it on, or unset it.")
	}
}
