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
	return fmt.Sprintf("This is not a template document: %s.", detail)
}

func templateVersionUnsupportedMsg(what string) string {
	return fmt.Sprintf("%s is not a template format this server reads. Use api_version wardyn/v1 and kind RunTemplate.", what)
}

func templateFieldUnknownMsg(path string) string {
	return fmt.Sprintf("%s is not a template field. Remove it, or check the spelling.", path)
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
	return fmt.Sprintf("%s cannot be used in a template on this server yet. Remove it for now: nothing was dropped.", path)
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
		return "Only an administrator of that group can publish templates to it."
	}
	return "A personal template belongs to the person who owns it."
}

func templateGroupUnverifiedMsg() string {
	return "Your group membership cannot be checked from this sign-in. Sign in again, then retry."
}

func templateNotFoundMsg() string {
	return "That template does not exist, or you cannot see it."
}

func templateRevisionConflictMsg(current int) string {
	return fmt.Sprintf("This template changed to revision %d since you opened it. Reload it, then apply your edit again.", current)
}

// templateDenialRefusal answers one TemplateAuthorize denial. A read that is
// denied is a missing template, and so is a write to a template the caller
// cannot see (another person's, a group they are not in): the id is never an
// existence oracle. A member of a group who is not its administrator, and
// anyone refused on org scope, learn only that they may not publish there.
func templateDenialRefusal(d types.TemplateDenial, owner types.TemplateOwner) *runRefusal {
	switch d {
	case types.TemplateAllowed:
		return nil
	case types.TemplateDenyGroupUnverified:
		return runError(http.StatusForbidden, reasonTemplateGroupUnverified, templateGroupUnverifiedMsg())
	case types.TemplateDenyNotOrgAdmin, types.TemplateDenyNotGroupAdmin:
		return runError(http.StatusForbidden, reasonTemplateScopeForbidden, templateScopeForbiddenMsg(string(owner.Scope)))
	}
	return runError(http.StatusNotFound, reasonTemplateNotFound, templateNotFoundMsg())
}
