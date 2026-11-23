// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"reflect"
	"sort"
	"testing"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// authenticatorMethods is every exported method of *Authenticator with whether it reads provider state, so a
// pending authenticator must act differently in it. A method that is not here fails the census until it is
// added and tagged. Each tagged true names the test that pins its pending behaviour.
var authenticatorMethods = map[string]struct {
	needsProvider bool
	pinnedBy      string
}{
	"AttachLoginGrantSink":       {},
	"AttachLogoutQueueSink":      {},
	"AttachPersonKeying":         {},
	"BackchannelLogoutEnabled":   {},
	"BackchannelLogoutHandler":   {true, "TestPending_BackchannelQueuedThenApplied"},
	"CallbackHandler":            {true, "TestPending_CallbackRefusedCookiesKept"},
	"CallbackHandlerWithDenials": {true, "TestPending_CallbackRefusedCookiesKept"},
	"ChartRoleMap":               {},
	"ClientID":                   {},
	"DefaultRole":                {},
	"DefaultRoleIsAdmin":         {},
	"DefaultRoleOutcome":         {},
	"Discover":                   {true, "TestPending_SetOnlyByUnreachable"},
	"DropUserView":               {},
	"EnforcesEmailVerified":      {},
	"FrontchannelFrameOrigin":    {},
	"FrontchannelLogoutEnabled":  {},
	"FrontchannelLogoutHandler":  {},
	"HasOperatorEmails":          {},
	"IsOperatorEmail":            {},
	"Issuer":                     {},
	"KeysPeopleByObjectID":       {},
	"LoginHandler":               {true, "TestPending_LoginRefusedNoCookies"},
	"LogoutHandler":              {},
	"LogoutQueueStats":           {},
	"MergedMapEmpty":             {},
	"Middleware":                 {},
	"NoteLogoutRateLimited":      {},
	"Pending":                    {},
	"PreviewRole":                {},
	"PreviewRoleAgainst":         {},
	"PurposeMAC":                 {},
	"RecordAttach":               {},
	"SessionBounds":              {},
	"SetUserView":                {},
	"SignOut":                    {},
	"UserViewStampedRole":        {},
	"UserViewSuperAdmin":         {},
	"VerifyActorToken":           {true, "TestPending_SubjectAndActorTokensInvalid"},
	"VerifySubjectToken":         {true, "TestPending_SubjectAndActorTokensInvalid"},
}

// P17: the exported method set of *Authenticator equals the pinned table, and every method that needs the
// provider names the pending test that pins it.
func TestAuthenticatorMethodCensus(t *testing.T) {
	typ := reflect.TypeOf((*writoidc.Authenticator)(nil))
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	var want []string
	for name, m := range authenticatorMethods {
		want = append(want, name)
		if m.needsProvider && m.pinnedBy == "" {
			t.Errorf("%s needs the provider but names no pending test", name)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the exported methods of *Authenticator changed: tag the new ones needsProvider or not, and pin the ones that do\n got  %v\n want %v", got, want)
	}
	if len(want) != 40 {
		t.Errorf("the table holds %d methods, want 40 (35 at v0.10.2 plus five)", len(want))
	}
}
