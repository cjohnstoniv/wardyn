// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The sentences of the template refusals. The console prints the server's
// sentence on a refusal and uses the same bytes for its own pre-checks
// (ui/src/app/lib/template-refusals.ts); TestTemplateRefusalSentencesMatchGolden
// pins both sides to one table (ui/src/app/lib/template-refusals.golden.json).
// Change a sentence here, in the TypeScript file and in the table together.

func templateDocumentInvalidMsg(detail string) string {
	return fmt.Sprintf("This document cannot be read: %s.", detail)
}

func templateVersionUnsupportedMsg(what string) string {
	return fmt.Sprintf("%s is not a document this server reads. Use api_version wardyn/v1 and kind Run or RunTemplate.", what)
}

// templateKindMismatchMsg says which kind a door reads and which the document
// is, for a document that is valid but of the other kind.
func templateKindMismatchMsg(want, got string) string {
	return fmt.Sprintf("This door reads a document of kind %s, and this one is kind %s. Change kind to %s.", want, got, want)
}

func templateFieldUnknownMsg(path string) string {
	return fmt.Sprintf("%s is not a field this document can hold. Remove it, or check the spelling.", path)
}

func templateFieldInvalidMsg(path, detail string) string {
	return fmt.Sprintf("%s %s.", path, detail)
}

func templateFieldExcludedMsg(path string) string {
	return fmt.Sprintf("%s is never part of a template: it belongs to one launch, or it is retired.", path)
}

func templateSecretRefusedMsg(path string) string {
	return fmt.Sprintf("%s holds what looks like a secret. A template keeps references to secrets, never their values.", path)
}

func templateRunStateRefusedMsg(path string) string {
	return fmt.Sprintf("%s is state of one run. A template holds reusable choices, not a run's history or authority.", path)
}

func templateMetadataInContentMsg(path string) string {
	return fmt.Sprintf("%s is set by the server from who saves the template. Remove it from the document.", path)
}

func templateFieldUnavailableMsg(path string) string {
	return fmt.Sprintf("%s cannot be used in a document on this server yet. Remove it for now: nothing was dropped.", path)
}

func templateDependencyMissingMsg(path, needs string) string {
	return fmt.Sprintf("%s needs %s. Include it, or list it under needs_setup.", path, needs)
}

func templateCoverageInvalidMsg(detail string) string {
	return fmt.Sprintf("The template's coverage does not fit its content: %s.", detail)
}

func templateSharedFieldRefusedMsg(path, scope string) string {
	return fmt.Sprintf("%s names something only one person has, so it cannot be in a %s template.", path, scope)
}

func templateScopeForbiddenMsg(scope string) string {
	switch types.TemplateScope(scope) {
	case types.TemplateScopeOrg:
		return "Only an organisation administrator can publish templates for the organisation."
	case types.TemplateScopeGroup:
		return "Only the template managers of that group can publish templates to it."
	}
	return "A personal template belongs to the person who owns it."
}

func templateGroupUnverifiedMsg() string {
	return "Your group membership cannot be checked from this sign-in. Sign in again, then retry."
}

func templateNotFoundMsg() string {
	return "That template does not exist, or you cannot see it."
}

func templatesUnavailableMsg() string {
	return "Templates are not available on this server yet."
}

func templateRevisionConflictMsg(current int) string {
	return fmt.Sprintf("This template changed to revision %d since you opened it. Reload it, then apply your edit again.", current)
}

// templateGrantOwnerRefusalMsg words one of resolveSecretOwner's refusals for
// the group template-manager grant; the reason stays the resolver's. outcome is
// what did not happen: "written" for a grant, "removed" for a revoke.
func templateGrantOwnerRefusalMsg(refusal, outcome string) string {
	if refusal == secretOwnerAmbiguousMsg {
		return "That matches more than one person, so no grant was " + outcome + ". Use the person's subject exactly, as the Audit log shows it."
	}
	msg := "That email address doesn't match anyone this deployment knows, so no grant was " + outcome + ". " +
		"Use the person's subject, as the Audit log shows it"
	if outcome == templateGrantRemoved {
		return msg + "."
	}
	return msg + ", or grant after they first sign in."
}

// The outcomes templateGrantOwnerRefusalMsg names.
const (
	templateGrantWritten = "written"
	templateGrantRemoved = "removed"
)

func templateNameTakenMsg() string {
	return "Another template available to the same people already uses this name."
}

func templateLimitReachedMsg(limit int) string {
	return fmt.Sprintf("This scope already holds %d templates, the most it can. Delete one first.", limit)
}

func templatePolicyFieldRefusedMsg(path string) string {
	return fmt.Sprintf("%s is not a network, credential or access rule. A policy holds only those; save a template instead.", path)
}

// templateDenialRefusal answers one TemplateAuthorize denial. A read that is
// denied is a missing template, and so is a write to a template the caller
// cannot see (another person's, a group they are not in): the id is never an
// existence oracle, and a read the caller's unverifiable group snapshot cannot
// answer is the same 404. A write that same snapshot cannot answer says so
// (403 group_unverified): the caller already holds the id. A member of a group
// who is not its administrator, and anyone refused on org scope, learn only
// that they may not publish there. No answer names the group.
func templateDenialRefusal(d types.TemplateDenial, action types.TemplateAction, owner types.TemplateOwner) *runRefusal {
	switch d {
	case types.TemplateAllowed:
		return nil
	case types.TemplateDenyGroupUnverified:
		if action == types.TemplateWrite {
			return runError(http.StatusForbidden, reasonTemplateGroupUnverified, templateGroupUnverifiedMsg())
		}
	case types.TemplateDenyNotOrgAdmin, types.TemplateDenyNotGroupAdmin:
		return runError(http.StatusForbidden, reasonTemplateScopeForbidden, templateScopeForbiddenMsg(string(owner.Scope)))
	}
	return runError(http.StatusNotFound, reasonTemplateNotFound, templateNotFoundMsg())
}
