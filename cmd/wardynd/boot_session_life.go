// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"
)

// sessionLifeFlags are the console session-lifetime behaviours (docs/ENV.md), each off by default;
// see the oidc.Config fields of the same names. A bootFlags built without registering them, as a
// test does, leaves the pointers nil, which reads as off.
type sessionLifeFlags struct {
	renewCap, idleTimeout                   *time.Duration
	silentReauth, loginHint, browserSession *bool
	backchannelLogout                       *bool
}

func registerSessionLifeFlags() sessionLifeFlags {
	return sessionLifeFlags{
		renewCap:          flagDuration("oidc-session-renew-cap", "WARDYN_OIDC_SESSION_RENEW_CAP", 0, "renew a console session on console activity, never past this long after sign-in (duration; 0 = off; at most 24h)"),
		idleTimeout:       flagDuration("oidc-session-idle-timeout", "WARDYN_OIDC_SESSION_IDLE_TIMEOUT", 0, "end a console session after this long with no console activity (duration; 0 = off)"),
		silentReauth:      flagBool("oidc-silent-reauth", "WARDYN_OIDC_SILENT_REAUTH", false, "a browser that signed in and has not signed out since first asks the identity provider with prompt=none, falling back once to the interactive sign-in (default false)"),
		loginHint:         flagBool("oidc-login-hint", "WARDYN_OIDC_LOGIN_HINT", false, "a browser that signed in sends that account as login_hint on its next sign-in, until it signs out (default false)"),
		browserSession:    flagBool("oidc-browser-session-cookie", "WARDYN_OIDC_BROWSER_SESSION_COOKIE", false, "send the console session cookie without an expiry, so the browser drops it when it closes (default false)"),
		backchannelLogout: flagBool("oidc-backchannel-logout", "WARDYN_OIDC_BACKCHANNEL_LOGOUT", false, "serve POST /auth/backchannel-logout, where the identity provider's signed logout token ends that person's console sessions (default false)"),
	}
}

// orZero is *p, or T's zero value (off) for a flag that was never registered.
func orZero[T any](p *T) (v T) {
	if p != nil {
		v = *p
	}
	return v
}

// validate refuses a negative WARDYN_OIDC_SESSION_IDLE_TIMEOUT, and a WARDYN_OIDC_SESSION_RENEW_CAP
// that is negative or above maxOIDCSessionTTL, for the reason that caps the session TTL.
func (s sessionLifeFlags) validate() error {
	if c := orZero(s.renewCap); c < 0 || c > maxOIDCSessionTTL {
		return fmt.Errorf("refusing to start: WARDYN_OIDC_SESSION_RENEW_CAP is %s; it must be 0 (off) or at most 24h", c)
	}
	if idle := orZero(s.idleTimeout); idle < 0 {
		return fmt.Errorf("refusing to start: WARDYN_OIDC_SESSION_IDLE_TIMEOUT is %s; it must be 0 (off) or positive", idle)
	}
	return nil
}
