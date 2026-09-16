// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-aws-sso is Wardyn's in-sandbox AWS SSO credential capture
// helper. It runs INSIDE the aws-sso container-login run (see
// internal/api/harnesscred.go's awsSSOAgent row) AFTER the operator completes
// `aws sso login --no-browser --use-device-code` in the attach terminal, and
// uploads the resulting SSO token cache to the control plane.
//
// Unlike the Anthropic container-login flow (which prints a token to the PTY
// for the operator to paste), `aws sso login` writes its credential to a FILE
// (~/.aws/sso/cache/<sha1>.json), so capture here is read-file-then-upload
// rather than terminal scraping — see harnesscred.go's captureViaHelper doc.
//
// Upload contract (mirrors wardyn-scan's brokered upload — the
// proxy injects the run token, so the sandbox NEVER holds it):
//
//	PUT ${WARDYN_PROXY_URL}/wardyn/v1/sso-token/${WARDYN_RUN_ID}
//	Content-Type: application/json
//	body: json(ssoBlob)  -- field tags match internal/api's awsSSOBlob
//
// forwarded by the proxy's brokered sso-token route to the control plane's
// PUT-authenticated /api/v1/internal/sso-token/{runID}, which rejects a runID
// that doesn't match the token's run (cross-run pollution guard) and requires
// the run to be the aws-sso harness-login run.
//
// Env:
//
//	WARDYN_PROXY_URL  proxy base URL (required)
//	WARDYN_RUN_ID     this run's id (required)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/sidecar"
)

// ssoCacheSubdir is where `aws sso login` writes the SSO token cache, and
// (per the AWS CLI) also where separate role-credential caches land — see
// ssoCacheFile.isSSOToken for how the two are told apart.
const ssoCacheSubdir = ".aws/sso/cache"

// resolveTimeout bounds the best-effort account/role lookup so a slow or
// hanging SSO portal call can never hang the upload.
const resolveTimeout = 15 * time.Second

func main() {
	if err := run(); err != nil {
		// Fail loud only on setup errors (missing env, no cache file to read).
		// Delivery failures are handled non-fatally inside run() (logged, exit
		// 0), matching wardyn-scan: a login run that can't
		// upload leaves nothing connected, an honest signal, without crashing
		// the throwaway run.
		fmt.Fprintln(os.Stderr, "wardyn-aws-sso:", err)
		os.Exit(1)
	}
}

func run() error {
	endpoint, err := proxyURL()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	cache, err := newestSSOToken(filepath.Join(home, ssoCacheSubdir))
	if err != nil {
		return err
	}
	blob, err := toBlob(cache)
	if err != nil {
		return err
	}
	// Best-effort account/role resolution. The control plane REQUIRES both:
	// awsSSOBlob.valid (internal/api/harnesscred.go) rejects a half-resolved
	// capture and internal/api/ssotoken.go answers 400 (pinned by
	// TestUploadSSOToken_HalfResolvedCaptureRejected), so a failure here means
	// the upload is refused and the operator re-runs the login — it is never a
	// fatal error in THIS process (see run()'s non-fatal upload branch).
	// This used to shell out to `aws sso list-accounts`/`list-account-roles
	// --access-token <token>`, which put the live SSO access token on a CHILD
	// PROCESS's own argv for the call's duration — readable by any /proc
	// reader in the sandbox sharing its PID namespace, not just same-uid
	// (F160). resolveAccountRole now calls the SSO portal API directly over
	// HTTP with the token in the x-amz-sso_bearer_token header instead, so it
	// never leaves this process's own memory for anywhere but that header —
	// no exec, no argv, ever.
	accountID, roleName, refusal, ok := pickAccountRole(blob.AccessToken, blob.Region, pinFromEnv())
	if refusal != "" {
		// A REFUSAL IS NOT A FAILURE TO RESOLVE. ok=false with no refusal is the
		// old best-effort miss (the upload goes out blank and the control plane
		// 400s it); a refusal means this sign-in cannot reach the identity this
		// deployment asked for, so nothing is uploaded at all — storing it would
		// bake the wrong account into every later run's ~/.aws/config.
		printFailure(refusal)
		return nil
	}
	if ok {
		blob.AccountID, blob.RoleName = accountID, roleName
	}
	body, err := json.Marshal(blob)
	if err != nil {
		printFailure("the captured session could not be encoded for upload: " + err.Error())
		return nil
	}
	if derr := sidecar.Upload(endpoint, body); derr != nil {
		fmt.Fprintln(os.Stderr, "wardyn-aws-sso: sso-token upload failed (non-fatal):", derr)
		printFailure(serverSentence(derr))
		return nil
	}
	// SUCCESS-ONLY marker, and a byte-for-byte contract: the setup UI's login pane
	// scrapes the attach PTY for exactly this line to end the login
	// (ui/.../harness-login-pane.tsx doneMarker). Printing it on a failed upload
	// would report a credential that was never stored.
	fmt.Fprintln(stdout, successMarker)
	return nil
}

// successMarker is the PTY line the UI waits for. Keep it in sync with
// harness-login-pane.tsx's LOGIN_FLOWS.aws doneMarker.
const successMarker = "wardyn: aws sso credential captured"

// stdout / stdin / stdinIsTerminal are the process's own streams, as VARIABLES
// so a test can drive the chooser and read the markers without a pty. Same
// seam shape as ssoPortalBase below; never reassigned outside tests.
var (
	stdout io.Writer = os.Stdout
	stdin  io.Reader = os.Stdin
	// stdinIsTerminal reports whether a person is actually watching this
	// stream. `aws sso login` runs on the operator's attach PTY, so stdin IS a
	// terminal in the real login sandbox; in CI, in a piped shell, and under
	// `go test` it is not, and a prompt nobody can answer must become a
	// refusal rather than a hang.
	stdinIsTerminal = func() bool {
		info, err := os.Stdin.Stat()
		return err == nil && info.Mode()&os.ModeCharDevice != 0
	}
)

// failMarker is the FAILURE counterpart of successMarker, and it exists because
// its absence was a bug: a refused upload logged to stderr and printed nothing
// on stdout, so the setup UI's login pane — which ends the login only on seeing
// a marker — span forever on a credential that had already been refused. Keep
// it in sync with harness-login-pane.tsx's LOGIN_FLOWS.aws failMarker
// (TestFailMarker_UIParity, the sibling of TestSuccessMarker_UIParity).
//
// NO TRAILING SPACE: the pane matches the marker as a prefix, and the sentence
// is joined to it with one space at print time.
const failMarker = "wardyn: aws sso credential rejected:"

// maxFailLineRunes caps the whole printed line. The sentence can come from the
// control plane (a refusal body) and lands on a terminal the operator is
// reading, so it is collapsed to ONE line and truncated rather than allowed to
// scroll the device code off the screen.
const maxFailLineRunes = 300

// ansiEscape matches an ESC-initiated terminal sequence: CSI (`ESC [ … final`),
// OSC (`ESC ] … BEL` or `ESC ] … ESC \`), and any other two-byte `ESC x`. The
// WHOLE sequence goes, payload included — stripping the ESC alone would leave
// "[31m" and "0;title" on screen as text.
var ansiEscape = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b.")

// defangMarkers neutralises a MARKER SPELLED IN UNTRUSTED TEXT. The login pane
// matches both markers by plain substring over the whole raw PTY buffer, so any
// value this helper echoes — a refusal sentence from the control plane, an AWS
// account name from the portal — that happens to contain the success marker
// would make the pane declare the login finished and tear the sandbox down,
// mid-chooser. Stripping control bytes does not help: the hazard is ordinary
// printable text. Replacing it does.
//
// Applied LAST, after whitespace collapsing, because collapsing can itself
// create a marker out of a value that did not literally contain one
// ("wardyn:  aws sso credential captured").
var defangMarkers = strings.NewReplacer(
	successMarker, "[marker removed]",
	failMarker, "[marker removed]",
)

// plainOneLine makes arbitrary text safe to print on the login terminal:
// escape sequences removed with their payloads, every remaining control byte
// turned into a space, whitespace collapsed, and either marker defanged.
//
// Everything it is applied to is UNTRUSTED FOR FORMATTING — a refusal body from
// the control plane, a transport error, an account or role name from the SSO
// portal — and the login pane matches the markers with a plain indexOf over the
// raw PTY buffer, so styling bytes could hide a marker exactly as a typo would,
// and a marker spelled inside a value could forge one.
func plainOneLine(s string) string {
	s = ansiEscape.ReplaceAllString(s, " ")
	s = strings.Map(func(r rune) rune {
		if r == 0x7f || (r < 0x20 && r != '\t' && r != '\n' && r != '\r') {
			return ' '
		}
		return r
	}, s)
	return defangMarkers.Replace(strings.Join(strings.Fields(s), " "))
}

// printFailure writes the fail marker and one sentence as a single PLAIN stdout
// line — no colour, no control bytes, no embedded newline — because the login
// pane matches it with a plain indexOf over the raw PTY buffer, the same
// guarantee the success marker already relies on.
//
// The LEADING newline is load-bearing: this can follow the chooser's prompt,
// which deliberately ends without one so the answer types on the same line. The
// pane matches the marker at the start of a line, so a refusal glued to
// "wardyn: account [1-2]: " would be invisible to it — the pane spinning
// forever, which is the exact bug this marker exists to fix.
func printFailure(sentence string) {
	line := failMarker + " " + plainOneLine(sentence)
	if r := []rune(line); len(r) > maxFailLineRunes {
		line = string(r[:maxFailLineRunes-1]) + "\u2026"
	}
	fmt.Fprintln(stdout, "\n"+line)
}

// serverSentence pulls the human sentence out of an upload error. sidecar.Upload
// wraps a non-2xx as `server returned <code>: <body>`, and the control plane's
// body is writeError's {"error":"..."} — so the operator reads the refusal the
// daemon actually wrote, not Go error plumbing around it. Anything that is not
// that shape is passed through raw: a wrong guess is worse than a verbose line.
func serverSentence(err error) string {
	raw := err.Error()
	brace := strings.Index(raw, "{")
	if brace < 0 {
		return raw
	}
	var body struct {
		Error string `json:"error"`
	}
	if jerr := json.Unmarshal([]byte(raw[brace:]), &body); jerr != nil || body.Error == "" {
		return raw
	}
	return body.Error
}

// ── DRAFT (M2 canon pending) ────────────────────────────────────────────────

// The refusals this helper prints after the fail marker, and the chooser's own
// prompt block. Every one of them is read by a person on the login terminal.
const (
	// DRAFT (M2 canon pending)
	pinAccountNotEntitledRefusal = "this deployment pins AWS sign-ins for this agent to account %s, which this sign-in does not reach — ask an admin to change the pin, or ask your cloud team for access to that account"
	// DRAFT (M2 canon pending)
	pinRoleNotInAccountRefusal = "this deployment pins AWS sign-ins for this agent to role %s in account %s, which this sign-in cannot assume there — ask an admin to change the pin, or ask your cloud team to grant you that role"
	// DRAFT (M2 canon pending)
	chooserNoTerminalRefusal = "this sign-in reaches more than one AWS account or role and there is no terminal here to choose on — ask an admin to pin the account and role on the agent row; this session reaches %s"
	// DRAFT (M2 canon pending)
	portalUnreachableRefusal = "the AWS access portal could not be reached — try the sign-in again"
	// DRAFT (M2 canon pending)
	chooserGaveUpRefusal = "nothing was chosen after three tries — ask an admin to pin the account and role on the agent row so this sign-in has nothing to guess"
)

// The chooser's prompt block, verbatim from the plan.
const (
	// DRAFT (M2 canon pending)
	chooserAccountsHeader = "wardyn: this sign-in reaches %d AWS accounts; choose the one your Bedrock model lives in."
	// DRAFT (M2 canon pending)
	chooserOptionLine = "  %d) %s  %s"
	// DRAFT (M2 canon pending)
	chooserAccountPrompt = "wardyn: account [1-%d]:"
	// DRAFT (M2 canon pending)
	chooserRolesHeader = "wardyn: choose the role your runs should assume in account %s."
	// DRAFT (M2 canon pending)
	chooserRolePrompt = "wardyn: role [1-%d]:"
)

// ssoPin is the admin's roster pin, delivered to this sandbox as launch env by
// launchHarnessLoginRun (internal/api/harnesscred.go). Both halves or neither:
// pinning the account alone still leaves the role picked for whoever signs in,
// which is the same defect one level down.
type ssoPin struct{ accountID, roleName string }

func (p ssoPin) set() bool { return p.accountID != "" && p.roleName != "" }

// The env names the daemon delivers the pin in. Named constants rather than
// inline literals so TestPinEnvVarParity can compare them against
// internal/api/awssso_pin.go's own pair — nothing else ties the two packages
// together, and a rename on one side silently stops delivering the pin.
const (
	awsSSOPinAccountEnv = "WARDYN_AWS_SSO_ACCOUNT_ID"
	awsSSOPinRoleEnv    = "WARDYN_AWS_SSO_ROLE_NAME"
)

func pinFromEnv() ssoPin {
	return ssoPin{
		accountID: os.Getenv(awsSSOPinAccountEnv),
		roleName:  os.Getenv(awsSSOPinRoleEnv),
	}
}

// proxyURL mirrors sidecar.ProxyRunURL's env validation, but targets the
// plain-noun sso-token route (like /wardyn/v1/recordings/) rather than the
// *-results/ convention: this PUTs a single captured credential, not a
// derived-facts result.
func proxyURL() (string, error) {
	base := strings.TrimRight(os.Getenv("WARDYN_PROXY_URL"), "/")
	if base == "" {
		return "", fmt.Errorf("WARDYN_PROXY_URL is required")
	}
	runID := os.Getenv("WARDYN_RUN_ID")
	if runID == "" {
		return "", fmt.Errorf("WARDYN_RUN_ID is required")
	}
	return base + "/wardyn/v1/sso-token/" + runID, nil
}

// ssoCacheFile is the JSON shape of an AWS CLI SSO TOKEN cache file
// (~/.aws/sso/cache/<sha1>.json).
type ssoCacheFile struct {
	AccessToken           string `json:"accessToken"`
	RefreshToken          string `json:"refreshToken"`
	ClientID              string `json:"clientId"`
	ClientSecret          string `json:"clientSecret"`
	StartURL              string `json:"startUrl"`
	Region                string `json:"region"`
	ExpiresAt             string `json:"expiresAt"`
	RegistrationExpiresAt string `json:"registrationExpiresAt"`
	// AccessKeyID is present only on a ROLE-credential cache file (the same
	// directory also holds those), never on an SSO token cache file — the
	// discriminator in isSSOToken.
	AccessKeyID string `json:"accessKeyId"`
}

// isSSOToken reports whether f is an SSO TOKEN cache file (has an access
// token bound to a start URL / region) rather than a role-credential cache
// file (has accessKeyId, no accessToken).
func (f ssoCacheFile) isSSOToken() bool {
	return f.AccessToken != "" && (f.StartURL != "" || f.Region != "") && f.AccessKeyID == ""
}

// ssoBlob is the JSON body PUT to the control plane. Field tags match
// internal/api's awsSSOBlob exactly — that JSON shape IS the contract between
// this sandbox helper and the control plane; there is deliberately no shared
// Go type across that trust boundary.
type ssoBlob struct {
	AccessToken           string    `json:"access_token"`
	RefreshToken          string    `json:"refresh_token,omitempty"`
	ClientID              string    `json:"client_id,omitempty"`
	ClientSecret          string    `json:"client_secret,omitempty"`
	StartURL              string    `json:"start_url"`
	Region                string    `json:"region"`
	AccountID             string    `json:"account_id,omitempty"`
	RoleName              string    `json:"role_name,omitempty"`
	ExpiresAt             time.Time `json:"expires_at"`
	RegistrationExpiresAt time.Time `json:"registration_expires_at,omitempty"`
}

// newestSSOToken reads dir and returns the most-recently-modified SSO TOKEN
// cache file's contents, skipping role-credential cache files and anything
// unparseable.
func newestSSOToken(dir string) (ssoCacheFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ssoCacheFile{}, fmt.Errorf("read sso cache dir %s: %w", dir, err)
	}
	var best ssoCacheFile
	var bestMod time.Time
	found := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		var f ssoCacheFile
		if jerr := json.Unmarshal(raw, &f); jerr != nil || !f.isSSOToken() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if !found || info.ModTime().After(bestMod) {
			best, bestMod, found = f, info.ModTime(), true
		}
	}
	if !found {
		return ssoCacheFile{}, fmt.Errorf("no SSO token cache file found under %s (run `aws sso login` first)", dir)
	}
	return best, nil
}

// toBlob converts the raw cache file into the upload shape, parsing the AWS
// CLI's non-standard timestamp format (see parseSSOTime).
func toBlob(f ssoCacheFile) (ssoBlob, error) {
	expiresAt, err := parseSSOTime(f.ExpiresAt)
	if err != nil {
		return ssoBlob{}, fmt.Errorf("parse expiresAt: %w", err)
	}
	var regExpiresAt time.Time
	if f.RegistrationExpiresAt != "" {
		if regExpiresAt, err = parseSSOTime(f.RegistrationExpiresAt); err != nil {
			return ssoBlob{}, fmt.Errorf("parse registrationExpiresAt: %w", err)
		}
	}
	return ssoBlob{
		AccessToken: f.AccessToken, RefreshToken: f.RefreshToken,
		ClientID: f.ClientID, ClientSecret: f.ClientSecret,
		StartURL: f.StartURL, Region: f.Region,
		ExpiresAt: expiresAt, RegistrationExpiresAt: regExpiresAt,
	}, nil
}

// parseSSOTime parses the AWS CLI's SSO cache timestamps. v2.31.13 (the version
// pinned in deploy/images/aws-sso/Dockerfile) writes plain RFC3339 ("...Z") —
// verified against a real cache file in TestParseRealAWSCLICacheFile. Older
// botocore wrote a LITERAL "UTC" suffix ("2021-05-14T18:59:22UTC"), which is
// still accepted so an older CLI in a corp mirror doesn't break capture.
func parseSSOTime(s string) (time.Time, error) {
	if strings.HasSuffix(s, "UTC") {
		if t, err := time.Parse("2006-01-02T15:04:05", strings.TrimSuffix(s, "UTC")); err == nil {
			return t.UTC(), nil
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", s)
}

// awsEndpointURLSSOEnv is the AWS SDK/CLI's OWN variable for re-pointing the
// SSO portal service — not a Wardyn knob. The daemon puts it on this sandbox
// only under the gated test hatch (internal/api's ssoInjectEndpointEnv, reached
// through harnessLogin.loginEnv), and the SAME hatch replaces the regional
// portal host in this run's egress allowlist (ssoEgressHosts). A name kept in
// parity with the daemon's by TestEndpointOverrideEnvVarParity.
const awsEndpointURLSSOEnv = "AWS_ENDPOINT_URL_SSO"

// ssoPortalBase returns the AWS SSO portal API's base URL for region.
//
// It honours AWS_ENDPOINT_URL_SSO, which is how the device-code half of the
// login already reaches a fake: `aws sso login` is the real AWS CLI and reads
// that variable pair itself. This helper's portal reads are the only AWS calls
// in the login sandbox the CLI does NOT make, so without this they went to the
// real portal.sso host while the proxy's allowlist had been re-pointed at the
// fake — denied, and the capture refused with an empty account/role pair. The
// variable is absent on every real deployment, where this is the regional URL
// it always was.
//
// A var, so tests can also replace it wholesale (see
// TestRun_NeverInvokesAWSCLIForAccountRoleLookup and
// TestResolveAccountRole_LeavesBlankOnPortalFailure).
var ssoPortalBase = func(region string) string {
	regional := "https://portal.sso." + region + ".amazonaws.com"
	raw := strings.TrimRight(strings.TrimSpace(os.Getenv(awsEndpointURLSSOEnv)), "/")
	if raw == "" {
		return regional
	}
	// Fall back rather than dial whatever a mistyped value parses to: a bad
	// hatch must fail as "the real AWS was denied by the proxy", which names
	// the misconfiguration, and never as a request to nowhere.
	if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fmt.Fprintf(os.Stderr, endpointOverrideIgnoredLine+"\n", awsEndpointURLSSOEnv, raw, regional)
		return regional
	}
	return raw
}

// ── DRAFT (M2 canon pending) ────────────────────────────────────────────────

// endpointOverrideIgnoredLine is the ONE stderr line an unparseable
// AWS_ENDPOINT_URL_SSO gets. Args: the variable name, the offending value, the
// regional URL used instead.
//
// DRAFT (M2 canon pending)
const endpointOverrideIgnoredLine = "wardyn-aws-sso: ignoring %s=%q (not an absolute http(s) URL) — using %s"

// portalAccount is one entitlement ListAccounts returns.
type portalAccount struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
}

// pickAccountRole decides WHICH AWS identity this sign-in captures. It used to
// be `AccountList[0]` / `RoleList[0]` with the comment "good enough for a
// single-account SSO setup" — and that is finding 1: [0] of an unordered,
// externally-mutable list is not a way to choose an identity. A cloud team
// granting an unrelated entitlement inserted an element at index 0 and silently
// re-pointed the account every run in the deployment signed with, with no diff,
// no audit row and no warning.
//
// Four arms, in the order they are decided:
//
//  1. A PIN (the admin set sso_account_id + sso_role_name on the roster row) is
//     VERIFIED, never assumed: the account must be one this session reaches and
//     the role must exist IN THAT ACCOUNT. A pin that does not hold is a
//     refusal with nothing uploaded — falling back to [0] would be the original
//     defect wearing a pin.
//  2. Exactly one account with exactly one role is taken silently. This is the
//     single-account operator's path and it is byte-for-byte what it was.
//  3. Anything else, with a person on the other end of stdin, is CHOSEN. The
//     login sandbox runs on the operator's attach PTY, so there is a terminal
//     to ask on (ask 2 of the finding).
//  4. Anything else with nobody watching is a refusal that names what it
//     reaches and asks for a pin — never a guess.
//
// (accountID, roleName, "", true) picked; ("", "", refusal, false) refused —
// the caller uploads NOTHING and prints the refusal after the fail marker;
// ("", "", "", false) is the old best-effort miss (portal unreachable, empty
// list), where the caller uploads a blank pair and the control plane answers
// 400 as it always has.
func pickAccountRole(accessToken, region string, pin ssoPin) (accountID, roleName, refusal string, ok bool) {
	if accessToken == "" || region == "" {
		return "", "", "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	client := portalClient()
	base := ssoPortalBase(region)

	accounts, got := listAccounts(ctx, client, base, accessToken)
	if !got || len(accounts) == 0 {
		return "", "", "", false
	}
	if pin.set() {
		return verifyPin(ctx, client, base, accessToken, pin)
	}
	if len(accounts) == 1 {
		roles, _, rgot := listAccountRoles(ctx, client, base, accessToken, accounts[0].AccountID)
		if !rgot || len(roles) == 0 {
			return "", "", "", false
		}
		if len(roles) == 1 {
			return accounts[0].AccountID, roles[0], "", true
		}
		role, rrefusal, rok := chooseRole(accounts[0].AccountID, roles)
		return accounts[0].AccountID, role, rrefusal, rok
	}
	return chooseAccountRole(ctx, client, base, accessToken, accounts)
}

// portalClient builds the client for the AWS SSO PORTAL reads (ListAccounts /
// ListAccountRoles). It keeps http.DefaultTransport — and therefore
// $HTTP_PROXY — DELIBERATELY, and that is the opposite of what B11a-F13 asked
// of the other in-sandbox clients.
//
// The split is by destination, not by binary. portal.sso.<region>.amazonaws.com
// is an EXTERNAL endpoint: it must traverse the sandbox's one route out, so
// governance sees it and the allow-list decides it. This binary's CONTROL-PLANE
// call — the token-capture upload — is the one that must go direct, and it does
// not live here: it is sidecar.Upload, whose client sets Proxy: nil. That is
// where the wardyn-aws-sso half of B11a-F13 landed.
func portalClient() *http.Client {
	return &http.Client{Timeout: resolveTimeout}
}

// verifyPin proves the admin's pin is reachable by THIS session before it is
// used. Both halves are checked against the portal, and the role half is
// checked inside the PINNED account — ListAccountRoles is scoped to the
// account_id it is asked about, so a role that exists in some other account
// the person also reaches is not a match.
func verifyPin(ctx context.Context, client *http.Client, base, accessToken string, pin ssoPin) (accountID, roleName, refusal string, ok bool) {
	roles, status, got := listAccountRoles(ctx, client, base, accessToken, pin.accountID)
	if !got {
		// FAIL CLOSED EITHER WAY — nothing is uploaded and there is no [0]
		// fallback — but say which failure it was. A 4xx is the portal
		// refusing an account this session is not entitled to, which is the
		// admin's pin to fix; a 0 (no answer at all) or a 5xx is AWS having a
		// bad minute, and telling the person their pin is wrong would send
		// them to the wrong colleague.
		if status == 0 || status >= 500 {
			return "", "", portalUnreachableRefusal, false
		}
		return "", "", fmt.Sprintf(pinAccountNotEntitledRefusal, pin.accountID), false
	}
	if !slices.Contains(roles, pin.roleName) {
		return "", "", fmt.Sprintf(pinRoleNotInAccountRefusal, pin.roleName, pin.accountID), false
	}
	return pin.accountID, pin.roleName, "", true
}

// chooseAccountRole asks the person which account, then which role in it.
func chooseAccountRole(ctx context.Context, client *http.Client, base, accessToken string, accounts []portalAccount) (accountID, roleName, refusal string, ok bool) {
	if !stdinIsTerminal() {
		return "", "", fmt.Sprintf(chooserNoTerminalRefusal, accountList(accounts)), false
	}
	fmt.Fprintf(stdout, chooserAccountsHeader+"\n", len(accounts))
	for i, a := range accounts {
		// SANITISED, for the same reason the fail line is: these values come
		// from the SSO portal, and the login pane matches its markers with a
		// plain indexOf over the raw PTY buffer. An account whose name
		// contains the success marker would otherwise make the pane declare
		// the login done — and kill the run — while the chooser is still
		// waiting for an answer.
		printOptionLine(i+1, a.AccountID, a.AccountName)
	}
	idx, chosen := promptIndex(fmt.Sprintf(chooserAccountPrompt, len(accounts)), len(accounts))
	if !chosen {
		return "", "", chooserGaveUpRefusal, false
	}
	acct := accounts[idx]
	roles, _, got := listAccountRoles(ctx, client, base, accessToken, acct.AccountID)
	if !got || len(roles) == 0 {
		return "", "", "", false
	}
	if len(roles) == 1 {
		return acct.AccountID, roles[0], "", true
	}
	role, rrefusal, rok := chooseRole(acct.AccountID, roles)
	return acct.AccountID, role, rrefusal, rok
}

// chooseRole is the second half, also reachable directly when this session
// reaches ONE account but several roles in it — RoleList[0] there is the same
// unordered pick the finding is about, one level down.
func chooseRole(accountID string, roles []string) (roleName, refusal string, ok bool) {
	if !stdinIsTerminal() {
		return "", fmt.Sprintf(chooserNoTerminalRefusal, accountID+": "+strings.Join(roles, ", ")), false
	}
	fmt.Fprintf(stdout, chooserRolesHeader+"\n", plainOneLine(accountID))
	for i, role := range roles {
		printOptionLine(i+1, role, "")
	}
	idx, chosen := promptIndex(fmt.Sprintf(chooserRolePrompt, len(roles)), len(roles))
	if !chosen {
		return "", chooserGaveUpRefusal, false
	}
	return roles[idx], "", true
}

// printOptionLine writes one numbered chooser option. Both interpolated values
// are PORTAL-SUPPLIED, so both go through plainOneLine (see chooseAccountRole),
// and the line is right-trimmed because the role list passes an empty second
// value and two trailing spaces on every role line is just litter.
func printOptionLine(n int, value, note string) {
	line := fmt.Sprintf(chooserOptionLine, n, plainOneLine(value), plainOneLine(note))
	fmt.Fprintln(stdout, strings.TrimRight(line, " "))
}

// maxChooserTries bounds the prompt. Three bad or empty reads (a fat finger, a
// closed stream, a paste of the wrong thing) become the same refusal a
// terminal-less sandbox gets, rather than a loop nobody can leave.
const maxChooserTries = 3

// promptIndex reads a 1-based choice and returns its 0-based index.
func promptIndex(label string, n int) (int, bool) {
	for try := 0; try < maxChooserTries; try++ {
		fmt.Fprint(stdout, label+" ")
		line, err := readLine(stdin)
		if err != nil && line == "" {
			return 0, false
		}
		choice, cerr := strconv.Atoi(strings.TrimSpace(line))
		if cerr == nil && choice >= 1 && choice <= n {
			return choice - 1, true
		}
		if err != nil {
			return 0, false
		}
	}
	return 0, false
}

// readLine reads one line UNBUFFERED. A bufio.Reader would be the obvious
// choice and is the wrong one here: the account prompt and the role prompt are
// two separate reads of the SAME stream, and a per-prompt bufio.Reader swallows
// whatever the first one read ahead — so the role answer, already typed, was
// gone by the time the role prompt asked for it. A prompt reads a handful of
// bytes once, so one syscall per byte costs nothing.
//
// ponytail: byte-at-a-time; if this ever reads bulk input, hoist ONE shared
// bufio.Reader over stdin rather than reintroducing a per-call one.
func readLine(r io.Reader) (string, error) {
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return string(line), nil
			}
			line = append(line, buf[0])
		}
		if err != nil {
			return string(line), err
		}
	}
}

// accountList renders the entitlements for the no-terminal refusal, so the
// person forwarding that line to an admin is forwarding the account ids the
// admin has to choose between.
func accountList(accounts []portalAccount) string {
	out := make([]string, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, a.AccountID+" ("+a.AccountName+")")
	}
	// printFailure sanitises the whole sentence, so these portal values are
	// covered on this path too — stated here so it stays true if the caller
	// ever changes.
	return strings.Join(out, ", ")
}

// listAccounts / listAccountRoles are the two portal reads. The access token
// travels ONLY in the x-amz-sso_bearer_token header on a request this process
// makes itself — never on a child process's argv (F160: the retired `aws` CLI
// shellout put it there). Both report false on any failure; the CALLER decides
// what a failure means, which differs between the pin arm (the portal refusing
// a pinned account is the answer, not an outage) and the rest.
func listAccounts(ctx context.Context, client *http.Client, base, accessToken string) ([]portalAccount, bool) {
	var out struct {
		AccountList []portalAccount `json:"accountList"`
	}
	if _, ok := ssoPortalGET(ctx, client, base+"/assignment/accounts", accessToken, &out); !ok {
		return nil, false
	}
	return out.AccountList, true
}

// listAccountRoles returns the roles of ONE account, plus the portal's status
// so a caller can tell "not entitled" (4xx) from "could not ask" (0 / 5xx).
func listAccountRoles(ctx context.Context, client *http.Client, base, accessToken, accountID string) ([]string, int, bool) {
	var out struct {
		RoleList []struct {
			RoleName string `json:"roleName"`
		} `json:"roleList"`
	}
	rolesURL := base + "/assignment/roles?account_id=" + url.QueryEscape(accountID)
	status, ok := ssoPortalGET(ctx, client, rolesURL, accessToken, &out)
	if !ok {
		return nil, status, false
	}
	names := make([]string, 0, len(out.RoleList))
	for _, r := range out.RoleList {
		names = append(names, r.RoleName)
	}
	return names, status, true
}

// ssoPortalGET performs a GET against the SSO portal API with the SSO access
// token in the x-amz-sso_bearer_token header — the header ListAccounts/
// ListAccountRoles read it from (botocore sso/2019-06-10/service-2.json
// AccessTokenType, location=header; enforced by test/awsssofake.checkBearer).
// Reports false on any failure (request build, transport, non-2xx status,
// decode) — never panics, never exec's, never places accessToken anywhere
// but this request's own header.
//
// It also returns the HTTP STATUS, and 0 when the request never got an answer
// at all (build failure, transport error, timeout). Callers need that
// distinction to say the right thing: a 4xx on a pinned account means "this
// session is not entitled to it", while a 0 or a 5xx means the portal could not
// be reached — and telling a person their admin's pin is wrong because AWS had
// a bad minute sends them to the wrong colleague. A decode failure on a 2xx
// keeps its status, since the portal did answer.
// maxPortalResponseBytes caps what a portal answer may cost us. The bodies here
// are an account list and a role list — a few hundred entries of short JSON at
// the very outside — so this is a ceiling against a hostile or wedged endpoint
// streaming forever into json.Decode, not a sizing of the real payload. The
// server side of the same lane has had maxSSOTokenUploadBytes since F006; this
// is its in-sandbox twin. A truncated body simply fails to decode, which is
// already the "portal did answer, but not with what we asked for" path.
const maxPortalResponseBytes = 512 << 10 // 512 KiB

func ssoPortalGET(ctx context.Context, client *http.Client, rawURL, accessToken string, dst any) (status int, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("x-amz-sso_bearer_token", accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, false
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, maxPortalResponseBytes)).Decode(dst) == nil
}
