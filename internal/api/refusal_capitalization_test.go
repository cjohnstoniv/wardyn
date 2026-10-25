// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Reasons shared by several allowlist entries below.
const (
	whyRunRefusalClause = "a state or remedy clause spliced into mpRunRefusal (run_model_provider.go), never shown alone"
	whyLaneReason       = "the \"(%s)\" half of providers400Lane (workspace_providers.go), never shown alone"
	whyScript           = "a shell script or header template run in the sandbox, not a sentence"
	whyReasonToken      = "a recorded or audited reason token, spliced into a larger line by its reader"
	whyLogged           = "written to the operator log, never rendered to a person"
	whyAfterPrefix      = "always spliced after a field name or clause, so it is never a sentence's first word"
	whyGoBootError      = "a boot refusal returned from fmt.Errorf and logged by main, not an HTTP or console sentence"
	whyBrandMacOS       = "brand name macOS is always written with a lower-case m"
)

// sentenceConstFragments are named string consts that this guard deliberately
// does NOT require to start with a capital letter: each is composed into the
// MIDDLE of a larger sentence (after a colon, an em dash, or as a %s
// substitution) and is never itself shown as a sentence's first word —
// confirmed by reading every call site. May only shrink: promoting a fragment
// to a stand-alone sentence is exactly the #585 bug this guard exists to
// catch, so an entry is deleted once its const is fixed, not added to.
var sentenceConstFragments = map[string]string{
	"mpSubNotSignedIn":  "the %s state clause inside mpRunRefusal (run_model_provider.go), never shown alone",
	"mpSubNoStore":      "the %s state clause inside mpRunRefusal, same sentence",
	"mpSubNotPerson":    "the %s state clause inside mpRunRefusal, same sentence",
	"mpSubNoImage":      "the %s state clause inside mpRunRefusal, same sentence",
	"mpRunRemedySignIn": "the %s remedy clause inside mpRunRefusal, same sentence",
	"mpRunRemedyPerson": "the %s remedy clause inside mpRunRefusal, same sentence",

	"mpAZNotSignedIn": whyRunRefusalClause, "mpAZSignInEnded": whyRunRefusalClause, "mpAZNotCovered": whyRunRefusalClause,
	"mpAZNoStore": whyRunRefusalClause, "mpAZNotPerson": whyRunRefusalClause,
	"mpBRNotSignedIn": whyRunRefusalClause, "mpBRNoStore": whyRunRefusalClause, "mpBRNotPerson": whyRunRefusalClause,
	"mpBRUnset": whyRunRefusalClause, "mpBRPinned": whyRunRefusalClause, "mpBRPortal": whyRunRefusalClause,
	"mpBRRenewing": whyRunRefusalClause, "mpBRRemedyRetry": whyRunRefusalClause, "mpBRPersistFailed": whyRunRefusalClause,
	"mpRunRemedy": whyRunRefusalClause, "mpRunStateMissing": whyRunRefusalClause, "mpRunStateOff": whyRunRefusalClause,
	"mpRunStateNotServing": whyRunRefusalClause, "mpRunNoKey": whyRunRefusalClause, "mpRunNoToken": whyRunRefusalClause,
	"mpRunRemedyService": whyRunRefusalClause,

	"laneAppReason": whyLaneReason, "laneSSHReason": whyLaneReason, "adoSSHRetired": whyLaneReason, "adoPATRetired": whyLaneReason,

	"subjectNameReservedMsg":                         "always written after the field's name (\"principal: \", \"sub: \", \"owner: \"), " + whyAfterPrefix,
	"RetiredModelCredentialRefusal":                  "always written after \"<name> is a retired model credential: \" or \"; \" (secrets.go, policy.go), " + whyAfterPrefix,
	"egress_baseline.go:inlineBaselineRefusal.route": "local const in egress_baseline.go, written after \"...is a governance write: \", " + whyAfterPrefix,
	"setup_recording.go:recordingCheck.pg":           "local const in setup_recording.go, written after \"Point -recording-dir ... or \", " + whyAfterPrefix,
	"githubCLIBlockedOrgProblem":                     "the predicate of \"components[%d]: this component <problem> Ask your admin.\" (components_run.go), " + whyAfterPrefix,
	"kubectlCLISecondRowRefusal":                     "the problem half of componentRefused's \"components[%d]: <field path><problem>\", " + whyAfterPrefix,
	"azureCLISecondRowRefusal":                       "the problem half of componentRefused's \"components[%d]: <field path><problem>\", " + whyAfterPrefix,
	"krunManagedFilesUnverified":                     "written after \"cannot vouch for them: \" in the 201 warning (runs_dispatch_agentpolicy.go), " + whyAfterPrefix,
	"residentPATBrokered":                            "the detail of a placement refusal, written after \"This run was not launched on your own runner: \" and next to its lower-case siblings (run_resident_delivery.go, runs_dispatch_resident_mints.go), " + whyAfterPrefix,
	"residentADOGitPATRefused":                       "same as residentPATBrokered",
	"residentRunnerTooOld":                           "same as residentPATBrokered",
	"boundaryLocker":                                 "the locker's name inside \"<knob> is locked by <locker>; ask <contact>\", " + whyAfterPrefix,
	"componentAutonomySource":                        "the phrase an autonomy refusal names (\"... because <phrase> ...\"), " + whyAfterPrefix,
}

// sentenceConstNotSentences are named string consts that #585 never covered
// at all — its rule is "api + console" sentences, and these are something else
// entirely, lower-case on purpose. Shrinks over time; the only additions allowed are brand
// names that are always written lower-case (whyBrandMacOS).
var sentenceConstNotSentences = map[string]string{
	"EntraAuthorityOverrideWarn": `opens with the literal "wardynd: TEST HATCH ACTIVE" on purpose (own doc comment) — a boot log line, never rendered by the console or the API`,
	"EntraAuthorityOverrideRefusal": "a Go error string returned from fmt.Errorf, not an HTTP/console sentence — " +
		"staticcheck ST1005 refuses a capitalised one so it composes when wrapped with %w",
	"AWSSSOEndpointOverrideRefusal": "same as EntraAuthorityOverrideRefusal: " + whyGoBootError,

	"desktopListScript": whyScript, "desktopUploadScript": whyScript, "desktopDownloadScript": whyScript,
	"agentStopScript": whyScript, "agentVerifyScript": whyScript, "signInReadScript": whyScript,
	"redirectProbeScript": whyScript, "security_headers.go:securityHeaders.csp": "the Content-Security-Policy header template, not a sentence",

	"attachLeaseLostReason": "a WebSocket close reason, " + whyReasonToken,
	"maskFencedReason":      "the session.detach audit reason, " + whyReasonToken,
	"sshCapReasonChannels":  "an ssh.channel_rejected audit reason, " + whyReasonToken,
	"sshCapReasonSync":      "an ssh.channel_rejected audit reason, " + whyReasonToken,
	"desktopPauseSignin":    "a recorded pause reason the console splices into \"Paused: <reason>\" (recording-level.ts), " + whyReasonToken,

	"bedrockBearerNotRecorded": "opens with the retired grant kind bedrock-api-key, a wire name that is case-significant",
	"providers400DupID":        "opens with the field name id (a Go error from fmt.Errorf, ST1005); the name is case-significant",

	"withheldUnreadMsg":      "logged as \"api: \"+msg by loggedMsg and answered to the sandbox's own client, not to a person: " + whyLogged,
	"roleSnapshotWarnNote":   "a slog attribute of the role-mapping warning: " + whyLogged,
	"roleSnapshotWarnRemedy": "a slog attribute of the role-mapping warning: " + whyLogged,
	"sftpServerMissing":      "written to the SSH session's stderr and logged, led by the protocol name sftp",

	"macOSVMOnRemote": whyBrandMacOS, "macOSVMsOff": whyBrandMacOS,
}

// wireFieldPathRe matches a const whose text opens with a field path and a
// colon ("model_providers[%d].id: ...", "git[%d].lanes: ...",
// "storage.user_drive.disabled: ..."): the path is case-significant, so the
// sentence after it follows the repo's validation-message style, not the
// capital rule. It needs at least one [..] or .name segment: a bare word and a
// colon is prose ("error: the run cannot start") and is NOT exempt.
var wireFieldPathRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\[[^\]]*\]|\.[a-z0-9_%]+)+: `)

// wireBareLeads are the bare field or package names that may open a const as
// "<name>: ..." — every one is a request field, a config block or a log
// prefix, listed so a new prose lead like "error: ..." is not waved through.
var wireBareLeads = []string{
	"agent", "agents", "api", "components", "deny_placements", "drive", "egress", "email", "groups_snapshot_stale",
	"lanes", "model_provider", "model_providers", "require_inspectable_llm", "sponsor", "wardynd",
}

var wireBareLeadRe = regexp.MustCompile(`^(` + strings.Join(wireBareLeads, "|") + `): `)

// wireIdentLeadRe matches a const whose first word is a wire identifier with
// an underscore or dot in it ("max_holds must be...", "delivery.mode %q ...",
// "github.com repositories ..."): capitalising it would misspell the name.
var wireIdentLeadRe = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+|\.[a-z0-9]+)+[ ,]`)

// allowKey is the key a const has in the allowlists: its bare name for a
// package-level const, "file:func.name" for one local to a function, so a
// same-named const anywhere else is not exempt.
func allowKey(file, fn, name string) string {
	if fn == "" {
		return name
	}
	return file + ":" + fn + "." + name
}

// lowercaseSentenceConsts is the census behind
// TestRefusalSentenceConstsStartCapitalized: it walks every top-level and
// function-local string const of files and returns one message per
// sentence-shaped const that starts with a lower-case letter and is neither
// listed in the two allowlists above nor opened by a wire identifier. It also
// records, in seenFragment and seenNotSentence, which allowlisted consts it
// still finds lower-case, so a stale entry (deleted, or now capitalised)
// fails the test.
func lowercaseSentenceConsts(files []apiSource, seenFragment, seenNotSentence map[string]bool) []string {
	var bad []string
	check := func(file, fn string, vs *ast.ValueSpec) {
		if len(vs.Names) != 1 || len(vs.Values) != 1 {
			return
		}
		lit := leadingStringLiteral(vs.Values[0])
		if !looksLikeSentence(lit) {
			return
		}
		if r := []rune(lit)[0]; r < 'a' || r > 'z' {
			return
		}
		name := vs.Names[0].Name
		key := allowKey(file, fn, name)
		if _, ok := sentenceConstFragments[key]; ok {
			seenFragment[key] = true
			return
		}
		if _, ok := sentenceConstNotSentences[key]; ok {
			seenNotSentence[key] = true
			return
		}
		if wireFieldPathRe.MatchString(lit) || wireBareLeadRe.MatchString(lit) || wireIdentLeadRe.MatchString(lit) {
			return
		}
		bad = append(bad, file+": "+name+" = "+strconv.Quote(lit)+" — a user-visible sentence must start with a capital letter")
	}
	for _, f := range files {
		for _, d := range f.file.Decls {
			fn := ""
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.CONST {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						check(f.name, "", vs)
					}
				}
				continue
			}
			if fd, ok := d.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			} else {
				fn = "(package var)"
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if gd, ok := n.(*ast.GenDecl); ok && gd.Tok == token.CONST {
					for _, spec := range gd.Specs {
						if vs, ok := spec.(*ast.ValueSpec); ok {
							check(f.name, fn, vs)
						}
					}
				}
				return true
			})
		}
	}
	return bad
}

// TestRefusalSentenceConstsStartCapitalized is #585's guard, widened by #2662
// to EVERY non-test internal/api file: a lower-case sentence-shaped string
// const — top-level or local to a function — turns this red unless it is on
// one of the two reviewed allowlists above (each entry carries its reason) or
// opens with a wire identifier. A new lower-case refusal therefore fails by
// default instead of shipping because its file was never listed. The
// allowlists must not go stale either: an entry whose const no longer occurs
// sentence-shaped fails too.
//
// This is still a NAMED-CONST scanner: it cannot see a sentence written as an
// inline literal inside a function body (uigateway.go's writeError bodies,
// run_entry.go's attach-ticket refusal, access.go's lockout sentence) — those
// stay fixed by hand.
func TestRefusalSentenceConstsStartCapitalized(t *testing.T) {
	seenFragment := map[string]bool{}
	seenNotSentence := map[string]bool{}
	for _, msg := range lowercaseSentenceConsts(parseAPISources(t), seenFragment, seenNotSentence) {
		t.Error(msg)
	}
	for _, msg := range staleAllowlistEntries(seenFragment, seenNotSentence) {
		t.Error(msg)
	}
}

// staleAllowlistEntries names every allowlist entry whose const was not seen
// lower-case: deleted, renamed, or capitalised since (the fix the entry waited
// for), so the lists can only shrink.
func staleAllowlistEntries(seenFragment, seenNotSentence map[string]bool) []string {
	var stale []string
	for name := range sentenceConstFragments {
		if !seenFragment[name] {
			stale = append(stale, "sentenceConstFragments lists "+name+", which no longer occurs as a lower-case sentence-shaped const in internal/api — delete the entry")
		}
	}
	for name := range sentenceConstNotSentences {
		if !seenNotSentence[name] {
			stale = append(stale, "sentenceConstNotSentences lists "+name+", which no longer occurs as a lower-case sentence-shaped const in internal/api — delete the entry")
		}
	}
	return stale
}

// TestRefusalSentenceConstsStartCapitalized_PlantedViolation is the guard's
// twin: it hands the scanner a file that is on no list — lower-case sentence
// consts at package level, inside a function, behind a bare "error:" lead, and
// under the names route and csp in a function the allowlist does not name,
// plus the shapes the guard must let through — and proves the scanner reports
// exactly the lower-case sentences, so a green guard is not green because it
// stopped looking. It also proves an allowlist entry whose const has been
// capitalised is reported stale.
func TestRefusalSentenceConstsStartCapitalized_PlantedViolation(t *testing.T) {
	const planted = `package api

const (
	plantedRefusal   = "this thing cannot be done right now, try again later"
	plantedCapital   = "This one starts with a capital letter, so it is fine"
	plantedMachine   = "signin_busy"
	plantedFieldPath = "git[%d].lanes: a path that opens a validation message"
	plantedBareLead  = "agents: a listed field name that opens a validation message"
	plantedIdentLead = "max_holds must be between 0 and %d, got %d"
	plantedProse     = "error: the run cannot start right now"
	mpSubNoStore     = "the %s state clause inside mpRunRefusal, same sentence"
)

func plantedLocal() string {
	const inFunction = "a lower-case sentence that lives inside a function body"
	return inFunction
}

// route and csp are allowlisted only in the functions the list names; the
// same names in any other function or file are ordinary lower-case sentences.
func plantedRoute() string {
	const route = "the run cannot be started right now, try again later"
	const csp = "the run cannot be started with this policy, change it first"
	return route + csp
}
`
	parse := func(name, src string) []apiSource {
		f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return []apiSource{{name, f}}
	}
	seenFragment, seenNotSentence := map[string]bool{}, map[string]bool{}
	got := lowercaseSentenceConsts(parse("planted.go", planted), seenFragment, seenNotSentence)
	const tail = " — a user-visible sentence must start with a capital letter"
	want := []string{
		`planted.go: plantedRefusal = "this thing cannot be done right now, try again later"` + tail,
		`planted.go: plantedProse = "error: the run cannot start right now"` + tail,
		`planted.go: inFunction = "a lower-case sentence that lives inside a function body"` + tail,
		`planted.go: route = "the run cannot be started right now, try again later"` + tail,
		`planted.go: csp = "the run cannot be started with this policy, change it first"` + tail,
	}
	if len(got) != len(want) {
		t.Fatalf("scanner reported %d findings %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d = %q, want %q", i, got[i], want[i])
		}
	}
	if !seenFragment["mpSubNoStore"] {
		t.Error("an allowlisted lower-case const was not marked seen")
	}

	// An allowlisted const that has since been capitalised is not "seen", so
	// its entry is reported stale instead of lingering.
	seenFragment = map[string]bool{}
	lowercaseSentenceConsts(parse("fixed.go", `package api

const mpSubNoStore = "The %s state clause inside mpRunRefusal, same sentence"
`), seenFragment, map[string]bool{})
	stale := strings.Join(staleAllowlistEntries(seenFragment, map[string]bool{}), "\n")
	if !strings.Contains(stale, "sentenceConstFragments lists mpSubNoStore,") {
		t.Errorf("a capitalised allowlisted const was not reported stale:\n%s", stale)
	}
}

// looksLikeSentence excludes short machine tokens (reason codes, mechanism
// names, env var names) that are lower-case on purpose: a sentence has a
// space and is long enough to be prose, never a bare identifier like
// "signin_busy" or "entra_signin".
func looksLikeSentence(s string) bool {
	return len(s) > 15 && strings.Contains(s, " ")
}

// leadingStringLiteral returns the first quoted string in a `"a" + "b" + ...`
// concatenation chain (a plain BasicLit is its own one-element chain), or ""
// when the expression is not a string literal or a concatenation of ones —
// which is how a non-string const (an int, a struct) and a composed
// expression with no leading literal (a bare fmt.Sprintf call) are skipped.
func leadingStringLiteral(e ast.Expr) string {
	for {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind != token.STRING {
				return ""
			}
			s, err := strconv.Unquote(v.Value)
			if err != nil {
				return ""
			}
			return s
		case *ast.BinaryExpr:
			if v.Op != token.ADD {
				return ""
			}
			e = v.X
		default:
			return ""
		}
	}
}
