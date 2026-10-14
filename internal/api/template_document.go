// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
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

// runDocumentKeys are the top-level keys of a kind Run document: a draft of one
// run, which has no name, coverage or needs_setup of its own.
var runDocumentKeys = []string{"api_version", "kind", "intent"}

// templateDependencies are the edges a partial template must not cut: a field
// and the field it only means anything beside. Either side may be an
// inline_policy.<field>. The dependency is satisfied by being in the template or
// being listed under needs_setup. TestTemplateDependencyEdges holds every row.
var templateDependencies = []struct{ field, needs string }{
	{"devcontainer_ref", "devcontainer_repo"},
	{"model_provider", "agent"},
	{"tool_approvals", "agent"},
	{"seed_auto_tools", "agent"},
	{"inline_policy.tool_rules", "agent"},
	{"workload", "experience"},
	{"startup", "experience"},
	{"interactive_start", "interactive"},
	{"task_mode", "interactive"},
	{"inline_policy.ui_apps", "interactive"},
	{"runner_id", "placement"},
	{"runner_id", "runner_pool_id"},
}

// The single door. A template document is checked in exactly one way, however
// it arrives: as text (validateTemplateImport) or as a struct a handler decoded
// (validateTemplateDocument, which writes it back to bytes first). The
// struct-level checks below the door read a document the strict decoder has
// already accepted and are reachable only from validateTemplateBytes
// (TestTemplateStructValidatorsAreReachableOnlyThroughTheDoor). A save handler
// calls validateTemplateDocument and stores what it returns.

// validateTemplateImport reads one document the way every door does and
// returns it normalised, or every diagnostic. It stores nothing. kind is the
// document kind the caller expects, "" for either.
func validateTemplateImport(src []byte, format client.TemplateFormat, kind string) client.TemplateImportResult {
	return validateTemplateBytes(src, format, nil, kind)
}

// validateTemplateBytes is the one validator: decode strictly, then (when the
// text decodes) hold the whole document and the scope it is published to
// (owner, when given) to the rest. Every problem is reported together. A
// document of a kind other than want (when want is set) is refused at kind.
func validateTemplateBytes(src []byte, format client.TemplateFormat, owner *types.TemplateOwner, want string) client.TemplateImportResult {
	doc, diags := decodeTemplateDocument(src, format)
	if got := documentKindOf(src, format); want != "" && got != want && (got == client.TemplateDocumentKind || got == client.RunDocumentKind) {
		diags = append([]client.TemplateDiagnostic{{Path: "kind", Reason: reasonTemplateVersionUnsupported, Message: templateKindMismatchMsg(want, got)}}, diags...)
	}
	if len(diags) == 0 {
		diags = validateTemplateContentDecoded(doc)
		if owner != nil {
			diags = append(diags, validateTemplateScopeDecoded(doc, *owner)...)
		}
	}
	if len(diags) > 0 {
		return client.TemplateImportResult{Diagnostics: diags}
	}
	return client.TemplateImportResult{Document: &doc, Diagnostics: []client.TemplateDiagnostic{}}
}

// validateTemplateDocument is the save door: it holds a struct-shaped document,
// and the scope it is published to, to everything validateTemplateImport holds
// text to. It returns the normalised document to store. It saves a template, so
// a document of kind Run is refused.
func validateTemplateDocument(doc client.TemplateDocument, owner types.TemplateOwner) (client.TemplateDocument, []client.TemplateDiagnostic) {
	body, err := json.Marshal(doc)
	if err != nil {
		return doc, []client.TemplateDiagnostic{{Reason: reasonTemplateDocumentInvalid, Message: templateDocumentInvalidMsg("it cannot be written as JSON")}}
	}
	res := validateTemplateBytes(body, client.TemplateFormatJSON, &owner, client.TemplateDocumentKind)
	if res.Document == nil {
		return doc, res.Diagnostics
	}
	return *res.Document, nil
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
	kind := documentKind(top)
	keys := templateDocumentKeys
	if kind == client.RunDocumentKind {
		keys = runDocumentKeys
	}
	for _, key := range sortedKeys(top) {
		switch {
		case key == "title" || key == "description" && kind == client.RunDocumentKind:
			d.add(key, reasonTemplateFieldExcluded, templateFieldExcludedMsg(key))
		case !slices.Contains(keys, key):
			d.unknownKey("", key)
		}
	}
	d.readHeader(top, kind)
	if raw, ok := top["intent"]; ok {
		d.walkIntent(raw, "intent")
	} else {
		d.invalid("intent", "is required")
	}
	if kind != client.RunDocumentKind {
		d.readNeedsSetup(top["needs_setup"])
	}
	if len(d.diags) > 0 {
		return doc, d.diags
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		d.add("", reasonTemplateDocumentInvalid, templateDocumentInvalidMsg(err.Error()))
	}
	return doc, d.diags
}

// documentKindOf reads the kind of a document that may not decode cleanly, so a
// kind mismatch is reported beside the document's other problems.
func documentKindOf(src []byte, format client.TemplateFormat) string {
	data, err := templateSourceJSON(src, format)
	if err != nil {
		return ""
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return ""
	}
	return documentKind(top)
}

// documentKind is the document's kind when it is a string, "" otherwise.
func documentKind(top map[string]json.RawMessage) string {
	var kind string
	_ = json.Unmarshal(top["kind"], &kind)
	return kind
}

// readHeader reads the header keys by kind. A Run document has neither name nor
// coverage; any other kind is read as a RunTemplate, and a kind that is neither
// is reported at kind.
func (d *templateDecoder) readHeader(top map[string]json.RawMessage, kind string) {
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
	// unsupported reports a value this server does not read. A key that is
	// present but not a string was already reported as the wrong type.
	unsupported := func(path, what string) {
		raw, present := top[path]
		var s string
		switch {
		case !present:
			what = path + " (missing)"
		case json.Unmarshal(raw, &s) != nil:
			return
		default:
			what = cmp.Or(what, `""`)
		}
		d.add(path, reasonTemplateVersionUnsupported, templateVersionUnsupportedMsg(what))
	}
	if version := text("api_version", 32); version != client.TemplateDocumentVersion {
		unsupported("api_version", version)
	}
	text("kind", 32)
	if kind != client.TemplateDocumentKind && kind != client.RunDocumentKind {
		unsupported("kind", kind)
	}
	if kind != client.TemplateDocumentKind {
		return
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
		if r, ok := need["reason"]; ok {
			if json.Unmarshal(r, &reason) != nil || utf8.RuneCountInString(reason) > 200 || !validTemplateText(reason) {
				d.invalid(path+".reason", "must be text of at most 200 characters")
			} else {
				d.scanString(path+".reason", reason)
			}
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

// validateTemplateContentDecoded holds a decoded document to what needs the
// whole of it: coverage (a Run document is a draft and has none), dependencies, the request contract's own shape checks,
// and what this server cannot honour yet. Only the door calls it.
func validateTemplateContentDecoded(doc client.TemplateDocument) []client.TemplateDiagnostic {
	d := &templateDecoder{}
	req, err := doc.Intent.Request()
	if err != nil {
		d.invalid("intent", "does not read as a run request: "+err.Error())
		return d.diags
	}
	if doc.Kind != client.RunDocumentKind {
		d.checkCoverage(doc, req)
	}
	d.checkDependencies(doc)
	for _, check := range append([]func(createRunRequest) *runRefusal{runModeConflictRefusal}, runContractShapeChecks...) {
		if refusal := check(req); refusal != nil {
			d.add("intent", refusal.body.Reason, refusal.body.Error)
		}
	}
	d.checkAvailable(req)
	return d.diags
}

func (d *templateDecoder) checkCoverage(doc client.TemplateDocument, req createRunRequest) {
	if len(doc.Intent.Names()) == 0 {
		d.add("intent", reasonTemplateCoverageInvalid, templateCoverageInvalidMsg("the intent specifies no field"))
	}
	for i, need := range doc.NeedsSetup {
		if templateSpecifies(doc, need.Field) {
			d.invalid(fmt.Sprintf("needs_setup[%d].field", i), "is already in the template, so it needs no setup")
		}
	}
	if doc.Coverage != client.TemplateCoverageFull {
		return
	}
	if len(doc.NeedsSetup) > 0 {
		d.add("needs_setup", reasonTemplateCoverageInvalid, templateCoverageInvalidMsg("a full template lists nothing under needs_setup"))
	}
	for _, detail := range templateFullGaps(doc, req) {
		d.add("intent", reasonTemplateCoverageInvalid, templateCoverageInvalidMsg(detail))
	}
}

// templateFullGaps lists what a full template still leaves for use time: the
// choices the wizard requires and no default fills. The run's text (task) is
// written per run and is not one of them. TestTemplateFullCoverage holds each.
func templateFullGaps(doc client.TemplateDocument, req createRunRequest) []string {
	var gaps []string
	switch {
	case doc.Intent.Has("experience"):
		if req.Experience == client.ExperienceBackground && !doc.Intent.Has("workload") {
			gaps = append(gaps, "a full background template says whether it runs an agent task or a command (workload)")
		}
	case !doc.Intent.Has("interactive"):
		gaps = append(gaps, "a full template says whether the run is a background task or an interactive environment (experience)")
	case !req.Interactive:
		if !doc.Intent.Has("task_mode") {
			gaps = append(gaps, "a full background template says whether it runs an agent task or a command (task_mode)")
		} else if req.TaskMode != "exec" && !doc.Intent.Has("agent") {
			gaps = append(gaps, "a full agent-task template names its agent")
		}
	}
	if !runAttachesAnything(req) && !req.NoRepositoriesOrDrives {
		gaps = append(gaps, "a full template attaches a repository or drive, or says there are none (no_repositories_or_drives)")
	}
	return gaps
}

func (d *templateDecoder) checkDependencies(doc client.TemplateDocument) {
	listed := func(field string) bool {
		return slices.ContainsFunc(doc.NeedsSetup, func(n client.TemplateSetupNeed) bool { return n.Field == field })
	}
	for _, dep := range templateDependencies {
		if templateSpecifies(doc, dep.field) && !templateSpecifies(doc, dep.needs) && !listed(dep.needs) {
			path := "intent." + dep.field
			d.add(path, reasonTemplateDependencyMissing, templateDependencyMissingMsg(path, dep.needs))
		}
	}
}

// checkAvailable refuses a valid field this server cannot honour yet. Each case
// mirrors a case of unappliedFieldsRefusal (TestTemplateUnavailableMirrorsRunRefusal
// holds the two together), so a field the run refuses stays refused in a
// template even if its registry Pending flag is cleared early. Fields whose lane
// has not landed are refused earlier, by the decoder, through the flag.
func (d *templateDecoder) checkAvailable(req createRunRequest) {
	for _, path := range documentAvailabilityRefusal(req) {
		d.add("intent."+path, reasonTemplateFieldUnavailable, templateFieldUnavailableMsg("intent."+path))
	}
}

// documentAvailabilityRefusal lists the request paths a document or a preset
// cannot hold because this server cannot honour them yet. Both doors read it.
// placement: local is the run door's to judge at launch (stepLocalEligibility),
// as it is for a request sent directly.
func documentAvailabilityRefusal(req createRunRequest) []string {
	var paths []string
	if path, what := runModeUnavailable(req); what != "" {
		paths = append(paths, path)
	}
	if req.AllowedImage != "" {
		paths = append(paths, "allowed_image")
	}
	for i, ref := range req.Components {
		if ref.Builtin == types.ComponentBuiltinAzureDevOps {
			paths = append(paths, fmt.Sprintf("components[%d]", i))
		}
	}
	return paths
}

// validateTemplateScopeDecoded holds a decoded document to the scope it is published to.
// A field that names one person's own thing is refused in an organisation or
// group template: the people it is offered to cannot use it, and it would
// disclose it.
func validateTemplateScopeDecoded(doc client.TemplateDocument, owner types.TemplateOwner) []client.TemplateDiagnostic {
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

// templatePolicyKeys are the inline_policy keys a policy may hold: the
// carried, run-wide keys of the network, credential and access-rule parts,
// read from the registry. TestTemplatePolicyKeysArePinned holds the set.
func templatePolicyKeys() []string {
	var keys []string
	for _, r := range templatePolicyRules {
		policyPart := r.Part == client.TemplatePartEgress || r.Part == client.TemplatePartCredentials || r.Part == client.TemplatePartAccessRules
		if policyPart && r.Section == templateSectionRunWide && r.Excluded == "" && r.Pending == "" {
			keys = append(keys, r.Name)
		}
	}
	return keys
}

// templatePolicyKindRefusals holds a door-normalised document to the policy
// kind: its intent is inline_policy alone, holding only templatePolicyKeys, and
// it is partial with nothing left for setup (it is laid onto the user's
// defaults, so it never stands alone).
func templatePolicyKindRefusals(doc client.TemplateDocument) []client.TemplateDiagnostic {
	d := &templateDecoder{}
	refuse := func(path string) { d.add(path, reasonTemplatePolicyFieldRefused, templatePolicyFieldRefusedMsg(path)) }
	for _, name := range doc.Intent.Names() {
		if name != "inline_policy" {
			refuse("intent." + name)
		}
	}
	var spec map[string]json.RawMessage
	_ = json.Unmarshal(doc.Intent.Raw("inline_policy"), &spec)
	allowed := templatePolicyKeys()
	for _, key := range sortedKeys(spec) {
		if !slices.Contains(allowed, key) {
			refuse("intent.inline_policy." + key)
		}
	}
	if len(spec) == 0 {
		d.invalid("intent.inline_policy", "must hold at least one network, credential or access rule")
	}
	if doc.Coverage != client.TemplateCoveragePartial {
		d.invalid("coverage", "must be partial: a policy is laid onto your defaults")
	}
	if len(doc.NeedsSetup) > 0 {
		d.invalid("needs_setup", "must be empty for a policy")
	}
	return d.diags
}

// templateReasonStatus is the HTTP status of a template refusal reason. The
// storage lane's handlers answer through it.
func templateReasonStatus(reason string) int {
	switch reason {
	case reasonTemplateNotFound:
		return http.StatusNotFound
	case reasonTemplateRevisionConflict, reasonTemplateNameTaken, reasonTemplateLimitReached:
		return http.StatusConflict
	case reasonTemplatesUnavailable:
		return http.StatusNotImplemented
	case reasonTemplateScopeForbidden, reasonTemplateGroupUnverified:
		return http.StatusForbidden
	case reasonTemplateFieldUnavailable, reasonTemplateVersionUnsupported, reasonTemplateDependencyMissing,
		reasonTemplateCoverageInvalid, reasonTemplateSharedFieldRefused, reasonTemplatePolicyFieldRefused:
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadRequest
}
