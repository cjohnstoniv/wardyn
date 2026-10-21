// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// The 401 hint (design ATTACH §11.7) follows the 401's own reason, not /healthz. These are its lines.
const (
	hint401Default = "authenticate with --token or WARDYN_ADMIN_TOKEN/WARDYN_TOKEN (the compose quick-start's token is in deploy/compose/README.md)"
	// sign_in_required: no credential was sent to a deployment that signs people in with single sign-on.
	hintSignInCLI    = "run `wardyn login`"
	hintSignInBefore = "this organisation signs people in with single sign-on and has no admin token. Set WARDYN_TOKEN to a " +
		"personal API token (--token and WARDYN_ADMIN_TOKEN take one too; --token shows it in the process list): a " +
		"Wardyn admin can invite you to " +
		"create one, and you claim it under Your account in the console."
	// admin_token_not_configured: a bearer was sent that this organisation does not know.
	hintUnknownTokenTail = " is not one this organisation knows (unknown, revoked or expired)"
	hintUnknownTokenCLI  = "; it takes precedence over `wardyn login`; unset it to use your sign-in"
)

// authHintFor is what the hint needs from the command that ran: the organisation's URL, where the
// bearer came from (--token, WARDYN_ADMIN_TOKEN, WARDYN_TOKEN or tokenFromLogin; "" for none) and why a
// stored CLI sign-in could not be used. rootCmd sets it.
var authHintFor struct {
	url, tokenFrom string
	session        error
}

// unauthorizedHint is the line printed under a 401. /healthz is read, once and anonymously, only after
// the two reasons whose remedy depends on whether this organisation has CLI sign-in.
func unauthorizedHint(ae *sdk.APIError) string {
	switch {
	case authHintFor.session != nil:
		return authHintFor.session.Error()
	case authHintFor.tokenFrom == tokenFromLogin:
		return hintStoredRefused
	case ae.Reason == "sign_in_required":
		if cliSignIn(authHintFor.url) {
			return hintSignInCLI
		}
		return hintSignInBefore
	case ae.Reason == "admin_token_not_configured" && authHintFor.tokenFrom != "":
		line := "the token in " + authHintFor.tokenFrom + hintUnknownTokenTail
		if cliSignIn(authHintFor.url) {
			line += hintUnknownTokenCLI
		}
		return line
	}
	return hint401Default
}

// cliSignIn reads /healthz.cli_sign_in without a credential. Anything unreadable is false: an
// organisation older than CLI sign-in does not send it.
func cliSignIn(baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := orgClient(baseURL, 0).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var h struct {
		CLISignIn bool `json:"cli_sign_in"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode == http.StatusOK && json.Unmarshal(b, &h) == nil && h.CLISignIn
}
