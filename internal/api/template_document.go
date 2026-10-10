// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Template document validation: what an import or a save is held to before a
// document may become a draft or a catalogue entry. Decoding is
// template_decode.go's; this file adds what needs the whole document (coverage,
// dependencies, the scope it is published to) and refuses, with the path and
// the reason, what this server cannot honour yet.

var templateDocumentKeys = []string{"api_version", "kind", "name", "description", "coverage", "intent", "needs_setup"}

// templateDependencies are the edges a partial template must not cut: a field
// and the field it only means anything beside. The dependency is satisfied by
// being in the template or being listed under needs_setup.
var templateDependencies = []struct{ field, needs string }{
	{"devcontainer_ref", "devcontainer_repo"},
	{"model_provider", "agent"},
	{"tool_approvals", "agent"},
	{"seed_auto_tools", "agent"},
	{"interactive_start", "interactive"},
	{"runner_id", "placement"},
}

// validateTemplateImport reads one document the way every door does and
// returns it normalised, or every diagnostic. It stores nothing.
func validateTemplateImport(src []byte, format client.TemplateFormat) client.TemplateImportResult {
	doc, diags := decodeTemplateDocument(src, format)
	if len(diags) == 0 {
		diags = validateTemplateContent(doc)
	}
	if len(diags) > 0 {
		return client.TemplateImportResult{Diagnostics: diags}
	}
	return client.TemplateImportResult{Document: &doc, Diagnostics: []client.TemplateDiagnostic{}}
}

// decodeTemplateDocument reads the text into a document. Any diagnostic means
// the returned document must not be used.
func decodeTemplateDocument(src []byte, format client.TemplateFormat) (client.TemplateDocument, []client.TemplateDiagnostic) {
	var doc client.TemplateDocument
	d := &templateDecoder{}
	data, err := templateSourceJSON(src, format)
	if err == nil {
		err = checkTemplateJSON(data)
	}
	if err != nil {
		d.add("", reasonTemplateDocumentInvalid, templateDocumentInvalidMsg(err.Error()))
		return doc, d.diags
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		d.add("", reasonTemplateDocumentInvalid, templateDocumentInvalidMsg("it must be an object"))
		return doc, d.diags
	}
	for _, key := range sortedKeys(top) {
		if !slices.Contains(templateDocumentKeys, key) {
			d.unknownKey("", key)
		}
	}
	d.readHeader(top)
	if raw, ok := top["intent"]; ok {
		d.walkIntent(raw, "intent")
	} else {
		d.invalid("intent", "is required")
	}
	d.readNeedsSetup(top["needs_setup"])
	if len(d.diags) > 0 {
		return doc, d.diags
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		d.add("", reasonTemplateDocumentInvalid, templateDocumentInvalidMsg(err.Error()))
	}
	return doc, d.diags
}

func (d *templateDecoder) readHeader(top map[string]json.RawMessage) {
	text := func(key string, max int) string {
		raw, ok := top[key]
		if !ok {
			return ""
		}
		var s string
		switch {
		case json.Unmarshal(raw, &s) != nil:
			d.invalid(key, "must be a string")
		case utf8.RuneCountInString(s) > max || !validTemplateText(s):
			d.invalid(key, fmt.Sprintf("must be printable text of at most %d characters", max))
		default:
			d.scanString(key, s)
		}
		return s
	}
	version, kind := text("api_version", 32), text("kind", 32)
	if version != client.TemplateDocumentVersion || kind != client.TemplateDocumentKind {
		d.add("api_version", reasonTemplateVersionUnsupported, templateVersionUnsupportedMsg(strings.TrimSpace(version+" "+kind)))
	}
	text("name", maxTemplateNameRunes)
	text("description", maxTemplateDescriptionRune)
	var coverage client.TemplateCoverage
	if raw, ok := top["coverage"]; !ok || json.Unmarshal(raw, &coverage) != nil ||
		(coverage != client.TemplateCoverageFull && coverage != client.TemplateCoveragePartial) {
		d.invalid("coverage", "must be full or partial")
	}
}

func validTemplateText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return r < ' ' && r != '\n' && r != '\t' })
}

func (d *templateDecoder) readNeedsSetup(raw json.RawMessage) {
	if raw == nil {
		return
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		d.invalid("needs_setup", "must be a list")
		return
	}
	for i, item := range items {
		path := fmt.Sprintf("needs_setup[%d]", i)
		var need map[string]json.RawMessage
		if json.Unmarshal(item, &need) != nil {
			d.invalid(path, "must be an object")
			continue
		}
		for _, key := range sortedKeys(need) {
			if key != "field" && key != "reason" {
				d.unknownKey(path, key)
			}
		}
		var field, reason string
		if json.Unmarshal(need["field"], &field) != nil || !templateKnownField(field) {
			d.invalid(path+".field", "must name a template field, or inline_policy.<field>")
		}
		if r, ok := need["reason"]; ok && (json.Unmarshal(r, &reason) != nil || utf8.RuneCountInString(reason) > 200 || !validTemplateText(reason)) {
			d.invalid(path+".reason", "must be text of at most 200 characters")
		}
	}
}

// templateKnownField reports whether name is a field a template can specify or
// leave for later: an intent field, or inline_policy.<policy field>.
func templateKnownField(name string) bool {
	if policy, ok := strings.CutPrefix(name, "inline_policy."); ok {
		r, found := templateRule(templatePolicyRules, policy)
		return found && r.Excluded == ""
	}
	if name == "pool_id" {
		return true
	}
	r, found := templateRule(templateRequestRules, name)
	return found && r.Excluded == ""
}

// templateSpecifies reports whether the document's intent specifies name.
func templateSpecifies(doc client.TemplateDocument, name string) bool {
	if policy, ok := strings.CutPrefix(name, "inline_policy."); ok {
		var spec map[string]json.RawMessage
		_ = json.Unmarshal(doc.Intent.Raw("inline_policy"), &spec)
		_, has := spec[policy]
		return has
	}
	return doc.Intent.Has(name)
}

// validateTemplateContent holds a decoded document to what needs the whole of
// it: coverage, dependencies, the request contract's own shape checks, and
// what this server cannot honour yet.
func validateTemplateContent(doc client.TemplateDocument) []client.TemplateDiagnostic {
	d := &templateDecoder{}
	d.checkCoverage(doc)
	d.checkDependencies(doc)
	req, err := doc.Intent.Request()
	if err != nil {
		d.invalid("intent", "does not read as a run request: "+err.Error())
		return d.diags
	}
	for _, check := range runContractShapeChecks {
		if refusal := check(req); refusal != nil {
			d.add("intent", refusal.body.Reason, refusal.body.Error)
		}
	}
	d.checkAvailable(doc, req)
	return d.diags
}

func (d *templateDecoder) checkCoverage(doc client.TemplateDocument) {
	if len(doc.Intent.Names()) == 0 {
		d.add("intent", reasonTemplateCoverageInvalid, templateCoverageInvalidMsg("the intent specifies no field"))
	}
	if doc.Coverage == client.TemplateCoverageFull && len(doc.NeedsSetup) > 0 {
		d.add("needs_setup", reasonTemplateCoverageInvalid, templateCoverageInvalidMsg("a full template lists nothing under needs_setup"))
	}
	for i, need := range doc.NeedsSetup {
		if templateSpecifies(doc, need.Field) {
			path := fmt.Sprintf("needs_setup[%d].field", i)
			d.invalid(path, "is already in the template, so it needs no setup")
		}
	}
}

func (d *templateDecoder) checkDependencies(doc client.TemplateDocument) {
	listed := func(field string) bool {
		return slices.ContainsFunc(doc.NeedsSetup, func(n client.TemplateSetupNeed) bool { return n.Field == field })
	}
	for _, dep := range templateDependencies {
		if doc.Intent.Has(dep.field) && !doc.Intent.Has(dep.needs) && !listed(dep.needs) {
			path := "intent." + dep.field
			d.add(path, reasonTemplateDependencyMissing, templateDependencyMissingMsg(path, dep.needs))
		}
	}
}

// checkAvailable refuses a valid field this server cannot honour yet. Each case
// mirrors a case of unappliedFieldsRefusal (TestTemplateUnavailableMirrorsRunRefusal
// holds the two together), plus the fields whose lane has no carrier at all.
func (d *templateDecoder) checkAvailable(doc client.TemplateDocument, req createRunRequest) {
	refuse := func(path string) { d.add(path, reasonTemplateFieldUnavailable, templateFieldUnavailableMsg(path)) }
	if req.Placement == client.PlacementLocal {
		refuse("intent.placement")
	}
	if req.AllowedImage != "" {
		refuse("intent.allowed_image")
	}
	if req.Overrides != nil && len(req.Overrides.Items()) > 0 {
		refuse("intent.overrides")
	}
	for i, ref := range req.Components {
		if ref.Builtin != "" {
			refuse(fmt.Sprintf("intent.components[%d]", i))
		}
	}
	if doc.Intent.Has("pool_id") {
		refuse("intent.pool_id")
	}
	if p := req.InlinePolicy; p != nil && p.PushRules != nil && p.PushRules.DenyNewExecutables {
		refuse("intent.inline_policy.push_rules.deny_new_executables")
	}
}

// validateTemplateForScope holds a document to the scope it is published to.
// A field that names one person's own thing is refused in an organisation or
// group template: the people it is offered to cannot use it, and it would
// disclose it.
func validateTemplateForScope(doc client.TemplateDocument, owner types.TemplateOwner) []client.TemplateDiagnostic {
	if !owner.Valid() {
		return []client.TemplateDiagnostic{{Path: "scope", Reason: reasonTemplateScopeForbidden, Message: templateScopeForbiddenMsg(string(owner.Scope))}}
	}
	if owner.Scope == types.TemplateScopePerson {
		return nil
	}
	var diags []client.TemplateDiagnostic
	for _, name := range doc.Intent.Names() {
		if rule, ok := templateRule(templateRequestRules, name); ok && rule.PersonOnly {
			path := "intent." + name
			diags = append(diags, client.TemplateDiagnostic{Path: path, Reason: reasonTemplateSharedFieldRefused, Message: templateSharedFieldRefusedMsg(path, string(owner.Scope))})
		}
	}
	return diags
}

// templateIncludes lists the parts a document carries, sorted: the refinement
// group of each field it specifies, with inline_policy counted by the fields
// inside it.
func templateIncludes(doc client.TemplateDocument) []client.TemplatePart {
	parts := map[client.TemplatePart]bool{}
	for _, name := range doc.Intent.Names() {
		rule, ok := templateRule(templateRequestRules, name)
		switch {
		case name == "inline_policy":
			var spec map[string]json.RawMessage
			_ = json.Unmarshal(doc.Intent.Raw(name), &spec)
			for field := range spec {
				if r, found := templateRule(templatePolicyRules, field); found {
					parts[r.Part] = true
				}
			}
		case ok:
			parts[rule.Part] = true
		}
	}
	return sortedKeys(parts)
}

// templateReasonStatus is the HTTP status of a template refusal reason. The
// storage lane's handlers answer through it.
func templateReasonStatus(reason string) int {
	switch reason {
	case reasonTemplateNotFound:
		return http.StatusNotFound
	case reasonTemplateRevisionConflict:
		return http.StatusConflict
	case reasonTemplateScopeForbidden, reasonTemplateGroupUnverified:
		return http.StatusForbidden
	case reasonTemplateFieldUnavailable, reasonTemplateVersionUnsupported, reasonTemplateDependencyMissing,
		reasonTemplateCoverageInvalid, reasonTemplateSharedFieldRefused:
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadRequest
}
