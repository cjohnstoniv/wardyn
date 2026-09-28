// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

// detect.go is the bounded, names-only CONTENT lane of the scanner, layered
// on markers.go's filename-only egress rule. Three invariants:
//   - NAMES ONLY: every regex captures an identifier via an anchored group;
//     everything right of '='/':' is discarded before storage. Real .env
//     files are never opened — only presence is recorded.
//   - ADVISORY ONLY: extracted facts feed RequiredSecrets/ServicesNeeded/
//     SuggestedEgress/SecretFilesPresent for operator display, never grants,
//     injection, or the egress auto-union.
//   - UNTRUSTED UNTIL VALIDATED: detectContent runs in-sandbox for repo
//     scans, so DeriveProfile re-validates every field (charsets + caps, see
//     validateSecretNeeds & friends below) before anything is persisted.
//
// Detector-target files are marker-matched or outside unmappedBuildFiles, so
// none reach UnrecognizedSamples / the AI advisory fallback.

import (
	"bufio"
	"encoding/json"
	"io"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/gitremote"
	"github.com/cjohnstoniv/wardyn/internal/hostrules"
)

// Collection-side bounds (per scan unless noted). DeriveProfile enforces the
// final (smaller) caps; these just stop a hostile tree from bloating facts.
const (
	maxDetectLines  = 4000 // lines examined per file
	maxKeysPerFile  = 200  // keys captured per file
	collectSlack    = 4    // facts may hold cap*slack raw entries pre-validation
	maxSecretNeeds  = 100
	maxServices     = 32
	maxSuggested    = 64
	maxSecretFiles  = 32
	maxSecretPathLn = 256
	maxLeakFindings = 64
	maxBuildMemMiB  = 262144 // 256 GiB sanity ceiling for a detected build heap

	// Per-scan budgets for the lanes that read many files (env greps, k8s
	// YAML, leaked-value pass) rather than a fixed few, bounding total work
	// on a hostile/huge tree.
	maxSourceFilesScanned = 600
	maxYAMLFilesScanned   = 300
	maxLeakFilesScanned   = 800
)

var (
	// needNameRE: post-validation charset for a secret/config key name.
	// Broader than a shell env key (dots/dashes) for SealedSecret data keys
	// like "credentials.json", but rejects whitespace/ANSI/unicode tricks.
	needNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]{0,63}$`)
	// dotenvKeyRE: dotenv-template key. Group 1 = comment marker (commented
	// ⇒ optional integration), group 2 = key. Nothing past '=' is captured.
	dotenvKeyRE = regexp.MustCompile(`^\s*(#\s*)?(?:export\s+)?([A-Za-z_][A-Za-z0-9_]{0,63})\s*=`)
	// placeholderRE: Spring/Quarkus `${VAR}`/`${VAR:default}`. Uppercase-first
	// so `${server.port}`-style property refs don't match; group 2 non-empty
	// ⇒ a default exists ⇒ optional.
	placeholderRE = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]{0,63})(:[^}]*)?}`)
	// composeVarRE: compose interpolation `${VAR}`/`${VAR:-def}`/`${VAR?err}`/
	// `${VAR:?err}`. Group 2 starting with '?' or ':?' ⇒ hard-required.
	composeVarRE   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]{0,63})((?::?[-?])[^}]*)?}`)
	composeImageRE = regexp.MustCompile(`^\s*image:\s*["']?([A-Za-z0-9._/:@-]+)`)
	// sealedKeyRE: one key of a YAML mapping line inside spec.encryptedData
	// only; values are ciphertext and never captured.
	sealedKeyRE  = regexp.MustCompile(`^(\s+)([A-Za-z0-9._-]{1,64}):`)
	dockerFromRE = regexp.MustCompile(`(?i)^\s*FROM\s+(?:--platform=\S+\s+)?(\S+)`)
	dockerAsRE   = regexp.MustCompile(`(?i)\sAS\s+(\S+)\s*$`)

	// envAccessRE: env-var NAME read from source (getenv/os.environ/
	// process.env/import.meta.env/ENV[]/Deno.env.get). NAME is always group
	// 1 or 2; nothing else is captured.
	envAccessRE = regexp.MustCompile(`(?:getenv|environ\.get|environ|ENV|env\.get)\s*[\[(]\s*["']([A-Za-z_][A-Za-z0-9_]{0,63})["']|(?:process\.env|import\.meta\.env)\.([A-Za-z_][A-Za-z0-9_]{0,63})`)
	// secretRefKeyRE: k8s secretKeyRef/secretRef data KEY, block or inline
	// flow YAML. Captures `key:`'s value (the need name), never `name:`.
	secretRefKeyRE = regexp.MustCompile(`\bkey:\s*["']?([A-Za-z_][A-Za-z0-9._-]{0,63})`)
	secretRefRE    = regexp.MustCompile(`secretKeyRef|secretRef`)
	ciSecretRE     = regexp.MustCompile(`secrets\.([A-Za-z_][A-Za-z0-9_]{0,63})`)
	// mavenRepoURLRE / gradleRepoURLRE: declared artifact-repository URL,
	// SUGGESTED egress only. mavenRepoURLRE applies only inside a
	// <repository>/<pluginRepository> element so pom license/SCM/project
	// <url>s never match.
	mavenRepoURLRE  = regexp.MustCompile(`<url>\s*(https?://[^<\s]+)`)
	gradleRepoURLRE = regexp.MustCompile(`\burl\s*[=(]?\s*(?:uri\()?["'](https?://[^"']+)`)
	// heapXmxRE/heapNodeRE: build-heap ceiling, JVM -Xmx<N><unit> or Node
	// --max-old-space-size=<N> (MiB).
	heapXmxRE  = regexp.MustCompile(`-Xmx(\d+)([kKmMgG])`)
	heapNodeRE = regexp.MustCompile(`max-old-space-size=(\d+)`)
)

// sourceExts is the set of source-file extensions worth an env-access grep.
// Kept small; unknown extensions are skipped.
var sourceExts = map[string]struct{}{
	".java": {}, ".kt": {}, ".scala": {}, ".groovy": {},
	".ts": {}, ".tsx": {}, ".js": {}, ".jsx": {}, ".mjs": {}, ".cjs": {},
	".py": {}, ".rb": {}, ".go": {}, ".php": {}, ".rs": {}, ".ex": {}, ".exs": {},
}

// serviceByImagePrefix maps a compose image's base name to a service id; its
// values are the only service ids DeriveProfile accepts.
var serviceByImagePrefix = map[string]string{
	"postgres":      "postgres",
	"postgis":       "postgres",
	"mysql":         "mysql",
	"mariadb":       "mariadb",
	"redis":         "redis",
	"valkey":        "redis",
	"mongo":         "mongodb",
	"minio":         "minio",
	"rabbitmq":      "rabbitmq",
	"kafka":         "kafka",
	"elasticsearch": "elasticsearch",
	"opensearch":    "elasticsearch",
	"memcached":     "memcached",
	"nats":          "nats",
	"mailhog":       "mail",
	"mailcatcher":   "mail",
	"falkordb":      "falkordb",
	"coturn":        "turn",
}

// secretKindByPrefix classifies a key name into a coarse family (+ implied
// service for unambiguous DB/store families). First match wins, ordered
// most-specific first; deliberately small, unknown ⇒ "generic".
var secretKindByPrefix = []struct{ prefix, kind string }{
	{"DATABASE_URL", "database"},
	{"POSTGRES", "postgres"},
	{"MYSQL", "mysql"},
	{"MARIADB", "mariadb"},
	{"MONGO", "mongodb"},
	{"REDIS", "redis"},
	{"MINIO", "minio"},
	{"S3_", "s3"},
	{"AWS_", "aws"},
	{"SMTP", "smtp"},
	{"MAIL", "smtp"},
	{"OIDC", "oidc"},
	{"OAUTH", "oidc"},
	{"SESSION", "session"},
	{"JWT", "session"},
	{"SECRET_KEY", "session"},
	{"STRIPE", "stripe"},
	{"GITHUB", "github"},
	{"ANTHROPIC", "llm"},
	{"OPENAI", "llm"},
	{"CLAUDE", "llm"},
	{"TURN_", "turn"},
}

// serviceForKind maps a secret-kind to the backing service it unambiguously
// implies. Generic "database" implies nothing — the engine is unknown.
var serviceForKind = map[string]string{
	"postgres": "postgres",
	"mysql":    "mysql",
	"mariadb":  "mariadb",
	"mongodb":  "mongodb",
	"redis":    "redis",
	"minio":    "minio",
	"turn":     "turn",
}

// knownSecretKinds is the closed set DeriveProfile accepts from untrusted
// facts; anything else coerces to "generic". "deploy" = k8s SealedSecret/
// secretKeyRef key; "ci" = CI-workflow-only (not dev-required); "code" =
// source-read only (lower-confidence).
var knownSecretKinds = func() map[string]struct{} {
	m := map[string]struct{}{"generic": {}, "deploy": {}, "database": {}, "ci": {}, "code": {}}
	for _, e := range secretKindByPrefix {
		m[e.kind] = struct{}{}
	}
	return m
}()

// weakKinds lose to any other kind when the same name is seen twice (a real
// config/family classification beats a bare CI/code sighting).
var weakKinds = map[string]struct{}{"ci": {}, "code": {}, "generic": {}}

// platformEnvNames is the closed set of environment-variable NAMES that are
// platform/runtime plumbing, never an application secret. detectEnvAccess
// can't tell getenv("HOME") from getenv("STRIPE_SECRET_KEY") at the read
// site, and unfiltered these dominated the operator checklist (40+ rows/repo).
// ponytail: a fixed enum, not a rule engine — under-covering just surfaces one
// more low-priority row; add names as reported rather than chasing exhaustive.
var platformEnvNames = map[string]struct{}{
	// shell / POSIX
	"HOME": {}, "PATH": {}, "PWD": {}, "OLDPWD": {}, "SHELL": {}, "TERM": {},
	"LANG": {}, "LANGUAGE": {}, "TZ": {}, "TMPDIR": {}, "TEMP": {}, "TMP": {},
	"USER": {}, "LOGNAME": {}, "HOSTNAME": {}, "EDITOR": {}, "PAGER": {}, "SHLVL": {},

	// Windows
	"USERPROFILE": {}, "APPDATA": {}, "LOCALAPPDATA": {}, "PROGRAMDATA": {},
	"PROGRAMFILES": {}, "SYSTEMROOT": {}, "WINDIR": {}, "COMSPEC": {},
	"PATHEXT": {}, "USERNAME": {}, "COMPUTERNAME": {},

	// proxies
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "NO_PROXY": {}, "ALL_PROXY": {}, "FTP_PROXY": {},

	// node / vite / frontend build
	"NODE_ENV": {}, "MODE": {}, "DEV": {}, "PROD": {}, "SSR": {}, "BASE_URL": {},
	"NPM_REGISTRY": {}, "NODE_OPTIONS": {}, "NODE_PATH": {}, "PUBLIC_URL": {},

	// go
	"GOPATH": {}, "GOROOT": {}, "GOOS": {}, "GOARCH": {}, "GOBIN": {},
	"GOCACHE": {}, "GOFLAGS": {}, "GOPROXY": {}, "CGO_ENABLED": {},

	// python / java toolchain
	"PYTHONPATH": {}, "PYTHONHOME": {}, "PYTHONUNBUFFERED": {}, "VIRTUAL_ENV": {},
	"JAVA_HOME": {}, "JAVA_OPTS": {}, "CLASSPATH": {}, "MAVEN_OPTS": {}, "GRADLE_OPTS": {},

	// generic app config
	"CI": {}, "DEBUG": {}, "PORT": {}, "HOST": {}, "LOG_LEVEL": {}, "ENV": {}, "APP_ENV": {},
	"VERBOSE": {}, "FORCE_COLOR": {}, "NO_COLOR": {},
}

// isPlatformEnvName reports whether name is platform/runtime plumbing rather
// than an application secret: an exact (case-insensitive) hit in
// platformEnvNames, or a locale (LC_*) / XDG (XDG_*) variable — open-ended
// families a fixed enum can't cover member-by-member.
func isPlatformEnvName(name string) bool {
	upper := strings.ToUpper(name)
	if _, ok := platformEnvNames[upper]; ok {
		return true
	}
	return strings.HasPrefix(upper, "LC_") || strings.HasPrefix(upper, "XDG_")
}

// leakRule matches a well-known secret VALUE format; high-precision only, and
// only the rule NAME and location are ever recorded, never the matched bytes.
// Mirrors internal/contentscan/patterns.go's secretRules (kept local rather
// than reshaping that package's Span/Finding streaming API). Entropy scanning
// is deliberately omitted (too many false positives); ADO PATs have no
// raw-value rule (opaque base64 — low-precision; caught in credential-URL form).
type leakRule struct {
	kind string
	re   *regexp.Regexp
}

var leakRules = []leakRule{
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"github-token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[0-9A-Za-z]{36}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,}\b`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"stripe-secret-key", regexp.MustCompile(`\bsk_live_[0-9A-Za-z]{24,}\b`)},
	{"private-key-block", regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
	{"gcp-service-account", regexp.MustCompile(`"type"\s*:\s*"service_account"`)},
	{"gitlab-pat", regexp.MustCompile(`\bglpat-[0-9A-Za-z_-]{20,}\b`)},
	// Credential embedded in a URL (~/.git-credentials / remote-URL shape).
	// Password >=8 chars so doc placeholders like https://user:pass@host don't flag.
	{"git-credentials-url", regexp.MustCompile(`https?://[^/\s:@]+:[^/\s@]{8,}@[^/\s]+`)},
}

// leakSeverity ranks a leak kind by what an attacker can do with the value,
// highest first, since the findings list is capped and something must survive
// the cut — previously that was arrival order, so a committed private key
// could be dropped in favor of an early JWT. Keeping reusable long-lived
// credentials is the only defensible priority.
//
// 3 = long-lived, usable as-is, from anywhere. 2 = usable as-is but scoped
// and/or short-lived. 1 = secret-SHAPED, often a fixture or expired artifact.
// Unknown kinds rank 0 (already dropped by leakKinds; sorts last if reached).
var leakSeverity = map[string]int{
	"private-key-block":   3,
	"aws-access-key":      3,
	"gcp-service-account": 3,
	"git-credentials-url": 3,
	"github-token":        3,
	"gitlab-pat":          3,
	"stripe-secret-key":   3,
	"slack-token":         2,
	"google-api-key":      2,
	"jwt":                 1, // usually expired, often a fixture
}

// capLeakFindings sorts leak findings riskiest-first, applies the cap, and
// reports whether anything was dropped (stable sort keeps ties deterministic).
// truncated must never be silent: this package promises never to drop a
// suspected secret quietly, so callers stamp NeedsReview with it.
func capLeakFindings(in []LeakFinding) (out []LeakFinding, truncated bool) {
	slices.SortStableFunc(in, func(a, b LeakFinding) int {
		if sa, sb := leakSeverity[a.Kind], leakSeverity[b.Kind]; sa != sb {
			return sb - sa // higher severity first
		}
		if a.Path != b.Path {
			return strings.Compare(a.Path, b.Path)
		}
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind)
		}
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return strings.Compare(a.Source, b.Source)
	})
	if len(in) > maxLeakFindings {
		return in[:maxLeakFindings], true
	}
	return in, false
}

var leakKinds = func() map[string]struct{} {
	m := map[string]struct{}{}
	for _, r := range leakRules {
		m[r.kind] = struct{}{}
	}
	return m
}()

// collectState carries the facts pointer plus per-scan budgets for the
// many-file lanes, so a hostile/huge tree can't make the content lane an
// unbounded read.
type collectState struct {
	facts       *ScanFacts
	sourceFiles int
	yamlFiles   int
	leakFiles   int
}

func classifySecretKind(name string) string {
	upper := strings.ToUpper(name)
	for _, e := range secretKindByPrefix {
		if strings.HasPrefix(upper, e.prefix) {
			return e.kind
		}
	}
	return "generic"
}

// detectContent dispatches a walked file to its content detector(s); a
// non-target file just costs a few string checks. rel is slash-separated and
// pathSafe-checked.
func detectContent(rel, name, path string, st *collectState) {
	facts := st.facts
	switch {
	case isDotenvTemplate(name):
		detectDotenvTemplate(path, facts)
	case isDotenvReal(name):
		// PRESENCE ONLY: a real secrets file is never opened, not even for
		// leak scanning — the .env-present warning covers it.
		if len(facts.SecretFilesPresent) < maxSecretFiles*collectSlack {
			facts.SecretFilesPresent = append(facts.SecretFilesPresent, rel)
		}
		return
	case isSpringConfig(name):
		detectPlaceholders(path, facts)
	case isComposeFile(name):
		detectCompose(path, facts)
	case isSealedSecret(name):
		detectSealedSecret(path, facts)
	case isDockerfile(name):
		detectDockerfileFrom(path, facts)
		detectHeap(path, facts)
	case name == "pom.xml":
		detectMavenRepos(path, facts)
	case isGradleBuild(name):
		detectGradleRepos(path, facts)
	case name == "gradle.properties":
		detectHeap(path, facts)
	case name == "package.json":
		detectPackageJSON(path, facts)
	case name == "Makefile" || name == "makefile" || name == "GNUmakefile":
		detectMakefile(path, facts)
	case isCIWorkflow(rel):
		detectCISecrets(path, facts)
	case isKubeYAML(name):
		if st.yamlFiles < maxYAMLFilesScanned {
			st.yamlFiles++
			detectSecretRef(path, facts)
		} else {
			// Past budget: mark Truncated so confidence reflects the scan is
			// incomplete rather than "high" over a tree never fully walked.
			facts.Truncated = true
		}
	}

	// Source-code env-access grep, bounded by a per-scan file budget.
	if _, ok := sourceExts[ext(name)]; ok {
		if st.sourceFiles < maxSourceFilesScanned {
			st.sourceFiles++
			detectEnvAccess(path, facts)
		} else {
			facts.Truncated = true
		}
	}

	// Leaked-value scan over text config/source, never a real .env, bounded
	// by a per-scan file budget.
	if leakScannable(name) {
		if st.leakFiles < maxLeakFilesScanned {
			st.leakFiles++
			detectLeaks(rel, path, facts)
		} else {
			facts.Truncated = true
		}
	}
}

func ext(name string) string {
	return strings.ToLower(filepath.Ext(name))
}

func isGradleBuild(name string) bool {
	return name == "build.gradle" || name == "build.gradle.kts" ||
		name == "settings.gradle" || name == "settings.gradle.kts"
}

func isCIWorkflow(rel string) bool {
	return strings.Contains(rel, ".github/workflows/") &&
		(strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml"))
}

// isKubeYAML matches a plausible k8s manifest by extension; a non-k8s YAML
// just costs one bounded read since detectSecretRef only extracts when a
// secretKeyRef/secretRef token is present.
func isKubeYAML(name string) bool {
	if isComposeFile(name) || isSealedSecret(name) {
		return false // already handled with more specific detectors
	}
	e := ext(name)
	return e == ".yaml" || e == ".yml"
}

// leakScannable reports whether a file should be run past the leaked-value
// catalog: text config + source, never a real .env.
func leakScannable(name string) bool {
	if isDotenvReal(name) {
		return false
	}
	if _, ok := sourceExts[ext(name)]; ok {
		return true
	}
	switch ext(name) {
	case ".yaml", ".yml", ".json", ".properties", ".toml", ".ini", ".conf", ".cfg", ".txt", ".xml":
		return true
	}
	return isDotenvTemplate(name)
}

func isDotenvTemplate(name string) bool {
	if !strings.HasPrefix(name, ".env") {
		return false
	}
	return strings.HasSuffix(name, ".example") || strings.HasSuffix(name, ".sample") ||
		strings.HasSuffix(name, ".template") || strings.HasSuffix(name, ".dist")
}

// isDotenvReal matches .env and .env.<anything-not-a-template>, e.g.
// .env.local, .env.production. Deliberately NOT bare-prefix ".env" — that
// would swallow .envrc (a direnv marker, presence-only in markers.go).
func isDotenvReal(name string) bool {
	return name == ".env" || (strings.HasPrefix(name, ".env.") && !isDotenvTemplate(name))
}

func isSpringConfig(name string) bool {
	if !strings.HasPrefix(name, "application") {
		return false
	}
	return strings.HasSuffix(name, ".properties") || strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

func isComposeFile(name string) bool {
	switch name {
	case "docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml":
		return true
	}
	return false
}

func isSealedSecret(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "sealedsecret") &&
		(strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml"))
}

func isDockerfile(name string) bool {
	return name == "Dockerfile" || name == "Containerfile" || strings.HasPrefix(name, "Dockerfile.")
}

// eachLine opens path and calls fn for each line, bounded by maxDetectLines
// and the package's 1 MiB read cap. Errors fail safe to "no lines". fn
// returns false to stop early (per-file key cap hit).
func eachLine(path string, facts *ScanFacts, fn func(line string) bool) {
	f, err := gitremote.OpenRegular(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxFileBytes))
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	lines := 0
	for sc.Scan() {
		lines++
		if lines > maxDetectLines {
			facts.Truncated = true
			return
		}
		if !fn(sc.Text()) {
			facts.Truncated = true
			return
		}
	}
	// A line over the 64 KiB buffer cap stops Scan() silently (bufio.ErrTooLong
	// looks like EOF). Stamp Truncated so a file with one absurd line doesn't
	// scan as complete when a token past the cutoff was never seen.
	if sc.Err() != nil {
		facts.Truncated = true
	}
}

// keyBudget caps the number of keys a detectXxx scanner captures per file,
// shared by every detector that walks a file counting keys (dotenv,
// placeholders, compose, SealedSecret, env-access, secretKeyRef, CI secrets).
type keyBudget struct{ n int }

// hit records one more key and reports whether the per-file cap has been
// exceeded (caller should stop scanning the file when true).
func (b *keyBudget) hit() bool {
	b.n++
	return b.n > maxKeysPerFile
}

// addNeed appends a raw (unvalidated) SecretNeed, respecting the collection
// bound. Returns false when the per-scan bound is hit.
func addNeed(facts *ScanFacts, name, kind string, optional bool) bool {
	if len(facts.SecretRequirements) >= maxSecretNeeds*collectSlack {
		return false
	}
	facts.SecretRequirements = append(facts.SecretRequirements, SecretNeed{Name: name, Kind: kind, Optional: optional})
	return true
}

func detectDotenvTemplate(path string, facts *ScanFacts) {
	var kb keyBudget
	eachLine(path, facts, func(line string) bool {
		idx := dotenvKeyRE.FindStringSubmatchIndex(line)
		if idx == nil {
			return true
		}
		if kb.hit() {
			return false
		}
		commented := idx[2] >= 0 // group 1 (the '#') matched
		key := line[idx[4]:idx[5]]
		// RHS emptiness is the required/optional signal: commented = optional
		// example; uncommented KEY= with no value = must-supply; uncommented
		// KEY=<value> = a working default, so optional. Value bytes are never
		// captured.
		optional := commented
		if !commented {
			optional = strings.TrimSpace(line[idx[1]:]) != ""
		}
		return addNeed(facts, key, classifySecretKind(key), optional)
	})
}

func detectPlaceholders(path string, facts *ScanFacts) {
	var kb keyBudget
	eachLine(path, facts, func(line string) bool {
		for _, m := range placeholderRE.FindAllStringSubmatch(line, -1) {
			if kb.hit() {
				return false
			}
			// m[2] non-empty ⇒ `${VAR:default}` ⇒ a safe in-file default exists.
			if !addNeed(facts, m[1], classifySecretKind(m[1]), m[2] != "") {
				return false
			}
		}
		return true
	})
}

func detectCompose(path string, facts *ScanFacts) {
	var kb keyBudget
	eachLine(path, facts, func(line string) bool {
		if m := composeImageRE.FindStringSubmatch(line); m != nil {
			if svc, ok := serviceByImagePrefix[imageBaseName(m[1])]; ok {
				if len(facts.ServicesFound) < maxServices*collectSlack {
					facts.ServicesFound = append(facts.ServicesFound, svc)
				}
			}
			return true
		}
		for _, m := range composeVarRE.FindAllStringSubmatch(line, -1) {
			if kb.hit() {
				return false
			}
			// `${VAR?err}` / `${VAR:?err}` ⇒ compose refuses to start without
			// it ⇒ hard-required; everything else has a fallback ⇒ optional.
			required := strings.HasPrefix(m[2], "?") || strings.HasPrefix(m[2], ":?")
			if !addNeed(facts, m[1], classifySecretKind(m[1]), !required) {
				return false
			}
		}
		return true
	})
}

// imageBaseName reduces an image ref to its bare repo name:
// "quay.io/minio/minio:latest" → "minio", "postgres:16" → "postgres".
func imageBaseName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	if i := strings.IndexAny(ref, ":@"); i >= 0 {
		ref = ref[:i]
	}
	return strings.ToLower(ref)
}

// detectSealedSecret extracts the KEY names under spec.encryptedData of a
// kubeseal SealedSecret manifest. The values are ciphertext (and never
// captured regardless). Keys are deploy-time needs: kind "deploy", optional.
func detectSealedSecret(path string, facts *ScanFacts) {
	inBlock := false
	blockIndent := -1
	var kb keyBudget
	eachLine(path, facts, func(line string) bool {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			return true
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if !inBlock {
			if trimmed == "encryptedData:" {
				inBlock, blockIndent = true, indent
			}
			return true
		}
		if indent <= blockIndent {
			// Dedented: block over. Keep scanning — a multi-doc file can hold several.
			inBlock = false
			if trimmed == "encryptedData:" {
				inBlock, blockIndent = true, indent
			}
			return true
		}
		if m := sealedKeyRE.FindStringSubmatch(line); m != nil {
			if kb.hit() {
				return false
			}
			return addNeed(facts, m[2], "deploy", true)
		}
		return true
	})
}

// detectDockerfileFrom records base-image registry hosts as SUGGESTED egress
// only — a content-derived host must never auto-widen an allowlist. Skips
// multi-stage `FROM <alias>` lines via the stage-alias set.
func detectDockerfileFrom(path string, facts *ScanFacts) {
	stages := map[string]struct{}{}
	eachLine(path, facts, func(line string) bool {
		m := dockerFromRE.FindStringSubmatch(line)
		if m == nil {
			return true
		}
		if a := dockerAsRE.FindStringSubmatch(line); a != nil {
			stages[strings.ToLower(a[1])] = struct{}{}
		}
		ref := m[1]
		if _, isStage := stages[strings.ToLower(ref)]; isStage || strings.EqualFold(ref, "scratch") {
			return true
		}
		host := "docker.io" // unqualified refs resolve to Docker Hub
		if i := strings.Index(ref, "/"); i > 0 {
			if first := ref[:i]; strings.ContainsAny(first, ".:") || first == "localhost" {
				host = first
				if j := strings.Index(host, ":"); j > 0 {
					host = host[:j] // host-only; drop the port
				}
			}
		}
		addSuggestedHost(facts, host)
		return true
	})
}

func addSuggestedHost(facts *ScanFacts, host string) {
	if len(facts.SuggestedEgress) < maxSuggested*collectSlack {
		facts.SuggestedEgress = append(facts.SuggestedEgress, strings.ToLower(host))
	}
}

// detectEnvAccess extracts env-var NAMES read from source code. Code refs are
// lower-confidence than a declared config key (a fallback may exist), so they
// are advisory (kind "code", optional): they enrich the needs panel but never
// a required-secret row.
func detectEnvAccess(path string, facts *ScanFacts) {
	var kb keyBudget
	eachLine(path, facts, func(line string) bool {
		for _, m := range envAccessRE.FindAllStringSubmatch(line, -1) {
			name := m[1]
			if name == "" {
				name = m[2]
			}
			if name == "" {
				continue
			}
			if kb.hit() {
				return false
			}
			if !addNeed(facts, name, "code", true) {
				return false
			}
		}
		return true
	})
}

// detectSecretRef extracts k8s Secret DATA KEYS from secretKeyRef/secretRef
// blocks (multi-line or inline flow YAML): the `key:` value is the need name;
// the Secret's `name:` is deliberately not captured. Deploy-time, optional.
func detectSecretRef(path string, facts *ScanFacts) {
	armed := false // saw a secretKeyRef/secretRef token; next key: is a data key
	var kb keyBudget
	eachLine(path, facts, func(line string) bool {
		if !secretRefRE.MatchString(line) {
			// A block-form key a line or two after secretKeyRef is still a data key while armed.
			if armed {
				if m := secretRefKeyRE.FindStringSubmatch(line); m != nil {
					if kb.hit() {
						return false
					}
					armed = false
					return addNeed(facts, m[1], "deploy", true)
				}
				// Give up once past a non-key content line.
				if strings.TrimSpace(line) != "" && !strings.Contains(line, "name:") {
					armed = false
				}
			}
			return true
		}
		// Line mentions secretKeyRef/secretRef: capture inline `key:` (flow form)
		// or arm for the next lines.
		if m := secretRefKeyRE.FindStringSubmatch(line); m != nil {
			if kb.hit() {
				return false
			}
			return addNeed(facts, m[1], "deploy", true)
		}
		armed = true
		return true
	})
}

// detectCISecrets extracts `secrets.NAME` refs from a GitHub Actions workflow,
// kind "ci", optional — never a dev-required row. Dedupe keeps a real config
// detector's stronger classification if the same name appears there too.
func detectCISecrets(path string, facts *ScanFacts) {
	var kb keyBudget
	eachLine(path, facts, func(line string) bool {
		for _, m := range ciSecretRE.FindAllStringSubmatch(line, -1) {
			if m[1] == "GITHUB_TOKEN" {
				continue // auto-provided by Actions; never operator-supplied
			}
			if kb.hit() {
				return false
			}
			if !addNeed(facts, m[1], "ci", true) {
				return false
			}
		}
		return true
	})
}

// detectMavenRepos extracts artifact-repository URLs from a pom.xml — SUGGESTED
// egress only, scoped to <repository>/<pluginRepository> elements so pom
// license/SCM/project/organization <url>s never leak in.
func detectMavenRepos(path string, facts *ScanFacts) {
	depth := 0 // >0 while inside a <repository>/<pluginRepository> element
	eachLine(path, facts, func(line string) bool {
		l := strings.ToLower(line)
		if strings.Contains(l, "<repository>") || strings.Contains(l, "<pluginrepository>") {
			depth++
		}
		if depth > 0 {
			if m := mavenRepoURLRE.FindStringSubmatch(line); m != nil {
				if h := hostrules.HostOf(m[1]); h != "" {
					addSuggestedHost(facts, h)
				}
			}
		}
		if strings.Contains(l, "</repository>") || strings.Contains(l, "</pluginrepository>") {
			if depth > 0 {
				depth--
			}
		}
		return true
	})
}

// detectGradleRepos extracts maven { url '...' } repository hosts from a Gradle
// build/settings file, SUGGESTED egress only. Gradle repo blocks have no
// license-URL noise, so a plain `url "http..."` match is safe enough.
func detectGradleRepos(path string, facts *ScanFacts) {
	eachLine(path, facts, func(line string) bool {
		for _, m := range gradleRepoURLRE.FindAllStringSubmatch(line, -1) {
			if h := hostrules.HostOf(m[1]); h != "" {
				addSuggestedHost(facts, h)
			}
		}
		return true
	})
}

// detectHeap records the largest build-heap ceiling seen, normalized to MiB.
// Advisory only.
func detectHeap(path string, facts *ScanFacts) {
	eachLine(path, facts, func(line string) bool {
		for _, m := range heapXmxRE.FindAllStringSubmatch(line, -1) {
			if mib := xmxToMiB(m[1], m[2]); mib > facts.BuildMemoryMiB {
				facts.BuildMemoryMiB = mib
			}
		}
		for _, m := range heapNodeRE.FindAllStringSubmatch(line, -1) {
			if mib := atoiBounded(m[1]); mib > facts.BuildMemoryMiB {
				facts.BuildMemoryMiB = mib // already MiB
			}
		}
		return true
	})
}

// detectLeaks runs the leaked-value catalog over a file's content: it reads
// VALUES to recognize a secret-shaped token but records only path/kind/line,
// never the matched bytes.
func detectLeaks(rel, path string, facts *ScanFacts) {
	line := 0
	eachLine(path, facts, func(text string) bool {
		line++
		for _, rule := range leakRules {
			if rule.re.MatchString(text) {
				if len(facts.LeakFindings) >= maxLeakFindings*collectSlack {
					return false
				}
				facts.LeakFindings = append(facts.LeakFindings, LeakFinding{Path: rel, Kind: rule.kind, Line: line})
			}
		}
		return true
	})
}

// conventionalScriptKeys/conventionalMakeTargets are the only names setup-command
// synthesis cares about; only KEY PRESENCE is captured, never a script body
// (a hostile scripts.build must never become a command).
var conventionalScriptKeys = map[string]struct{}{"build": {}, "test": {}, "lint": {}}
var conventionalMakeTargets = map[string]struct{}{"build": {}, "test": {}, "install": {}, "lint": {}}

// makeTargetRE captures a Makefile target name at the start of a rule line.
var makeTargetRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*)\s*:`)

// detectPackageJSON records which conventional script KEYS a package.json
// declares, never the bodies. Parse failure is safe (no keys recorded).
func detectPackageJSON(path string, facts *ScanFacts) {
	f, err := gitremote.OpenRegular(path)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes))
	if err != nil {
		return
	}
	var pkg struct {
		Scripts map[string]json.RawMessage `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return
	}
	for k := range pkg.Scripts {
		if _, ok := conventionalScriptKeys[k]; ok && !slices.Contains(facts.ScriptKeys, k) {
			facts.ScriptKeys = append(facts.ScriptKeys, k)
		}
	}
}

// detectMakefile records which conventional TARGETS a Makefile defines —
// names only, never recipe bodies.
func detectMakefile(path string, facts *ScanFacts) {
	eachLine(path, facts, func(line string) bool {
		if m := makeTargetRE.FindStringSubmatch(line); m != nil {
			t := m[1]
			if _, ok := conventionalMakeTargets[t]; ok && !slices.Contains(facts.MakeTargets, t) {
				facts.MakeTargets = append(facts.MakeTargets, t)
			}
		}
		return true
	})
}

// jsInstallCmd returns the install command for the detected JS package manager.
func jsInstallCmd(pkgMgrs map[string]struct{}) (string, bool) {
	switch {
	case has(pkgMgrs, "pnpm"):
		return "pnpm install --frozen-lockfile", true
	case has(pkgMgrs, "yarn"):
		return "yarn install --frozen-lockfile", true
	case has(pkgMgrs, "npm"):
		return "npm ci", true
	}
	return "", false
}

func jsRunPrefix(pkgMgrs map[string]struct{}) string {
	if has(pkgMgrs, "pnpm") {
		return "pnpm run "
	}
	if has(pkgMgrs, "yarn") {
		return "yarn "
	}
	return "npm run "
}

func has(m map[string]struct{}, k string) bool { _, ok := m[k]; return ok }

// deriveSetupCommands synthesizes ordered install/build/test/lint commands from
// FIXED templates keyed on detected package managers/tools/script keys —
// never taken from file content. Deduped by (stage, command).
func deriveSetupCommands(pkgMgrs, tools map[string]struct{}, scriptKeys, makeTargets []string, hasMvnw, hasGradlew bool) []SetupCommand {
	var out []SetupCommand
	seen := map[string]struct{}{}
	add := func(stage, cmd, src string) {
		if cmd == "" {
			return
		}
		k := stage + "\x00" + cmd
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		out = append(out, SetupCommand{Stage: stage, Command: cmd, Source: src})
	}
	scriptSet := map[string]struct{}{}
	for _, k := range scriptKeys {
		scriptSet[k] = struct{}{}
	}
	makeSet := map[string]struct{}{}
	for _, t := range makeTargets {
		makeSet[t] = struct{}{}
	}

	// JavaScript / Node.
	if cmd, ok := jsInstallCmd(pkgMgrs); ok {
		add("install", cmd, "convention:node")
		pre := jsRunPrefix(pkgMgrs)
		if _, ok := scriptSet["build"]; ok {
			add("build", pre+"build", "package.json:build")
		}
		if _, ok := scriptSet["test"]; ok {
			add("test", pre+"test", "package.json:test")
		}
		if _, ok := scriptSet["lint"]; ok {
			add("lint", pre+"lint", "package.json:lint")
		}
	}
	// Python.
	if has(pkgMgrs, "poetry") {
		add("install", "poetry install", "convention:poetry")
		add("test", "poetry run pytest", "convention:poetry")
	} else if has(pkgMgrs, "uv") {
		add("install", "uv sync", "convention:uv")
		add("test", "uv run pytest", "convention:uv")
	} else if has(pkgMgrs, "pipenv") {
		add("install", "pipenv install --dev", "convention:pipenv")
		add("test", "pipenv run pytest", "convention:pipenv")
	} else if has(pkgMgrs, "pip") {
		// venv because: Debian's system Python is PEP-668 externally-managed and
		// the sandbox agent is non-root; prefer an editable install, falling back
		// to requirements.txt; SETUPTOOLS_SCM_PRETEND_VERSION lets scm-versioned
		// builds succeed on the shallow (tagless) clone.
		add("install",
			"python3 -m venv .venv && SETUPTOOLS_SCM_PRETEND_VERSION=0.0.0 .venv/bin/pip install -e . "+
				"|| SETUPTOOLS_SCM_PRETEND_VERSION=0.0.0 .venv/bin/pip install -r requirements.txt",
			"convention:pip")
		add("test", ".venv/bin/python -m pytest", "convention:pip")
	}
	// Go.
	if has(pkgMgrs, "go") {
		add("install", "go mod download", "convention:go")
		add("build", "go build ./...", "convention:go")
		add("test", "go test ./...", "convention:go")
	}
	// Rust.
	if has(pkgMgrs, "cargo") {
		add("build", "cargo build", "convention:cargo")
		add("test", "cargo test", "convention:cargo")
	}
	// Java: maven / gradle (prefer the wrapper when present).
	if has(pkgMgrs, "maven") {
		mvn := "mvn"
		if hasMvnw {
			mvn = "./mvnw"
		}
		add("build", mvn+" -B -q -DskipTests package", "convention:maven")
		add("test", mvn+" -B -q test", "convention:maven")
	}
	if has(pkgMgrs, "gradle") {
		gr := "gradle"
		if hasGradlew {
			gr = "./gradlew"
		}
		add("build", gr+" build -x test", "convention:gradle")
		add("test", gr+" test", "convention:gradle")
	}
	// Ruby.
	if has(pkgMgrs, "gem") {
		add("install", "bundle install", "convention:bundler")
	}
	// PHP.
	if has(pkgMgrs, "composer") {
		add("install", "composer install --no-interaction", "convention:composer")
	}
	// Docs generators: marker-derived tool hints → fixed build-stage templates.
	// Docusaurus has none — its package.json build script rides the JS branch above.
	if has(tools, "mkdocs") {
		add("build", "mkdocs build", "convention:mkdocs")
	}
	if has(tools, "sphinx") {
		add("build", "sphinx-build -b html docs _build", "convention:sphinx")
	}
	if has(tools, "hugo") {
		add("build", "hugo --minify", "convention:hugo")
	}

	// Makefile targets corroborate but never override a language convention:
	// add one only for a stage with no convention-provided command yet.
	stageHas := map[string]bool{}
	for _, c := range out {
		stageHas[c.Stage] = true
	}
	for _, stage := range []string{"install", "build", "test", "lint"} {
		if _, ok := makeSet[stage]; ok && !stageHas[stage] {
			add(stage, "make "+stage, "Makefile:"+stage)
		}
	}
	return out
}

// xmxToMiB converts a -Xmx numeric+unit to MiB (k/m/g), bounded.
func xmxToMiB(numStr, unit string) int {
	n := atoiBounded(numStr)
	switch strings.ToLower(unit) {
	case "k":
		return n / 1024
	case "g":
		if n > maxBuildMemMiB/1024 {
			return maxBuildMemMiB
		}
		return n * 1024
	default: // m
		return n
	}
}

// atoiBounded parses a non-negative int (untrusted input), capping at
// maxBuildMemMiB and failing safe to 0 on overflow/garbage.
func atoiBounded(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > maxBuildMemMiB {
			return maxBuildMemMiB
		}
	}
	return n
}

// --- DeriveProfile-side validation (facts are untrusted input) ---

// validateSecretNeeds drops non-conforming names, coerces unknown kinds to
// "generic", dedupes by name (required beats optional; a real config kind
// beats a weak ci/code/generic sighting), caps and sorts for determinism.
func validateSecretNeeds(raw []SecretNeed) []SecretNeed {
	byName := make(map[string]SecretNeed, len(raw))
	for _, n := range raw {
		name := strings.TrimSpace(n.Name)
		if !needNameRE.MatchString(name) {
			continue
		}
		// Filtered here, not at collection time, since this is the one boundary
		// both scan lanes cross: host-side Scan and the repo-lane's re-derivation
		// of untrusted uploaded facts via DeriveProfile. A collection-site filter
		// would miss the latter.
		if isPlatformEnvName(name) {
			continue
		}
		kind := n.Kind
		if _, ok := knownSecretKinds[kind]; !ok {
			kind = "generic"
		}
		if prev, seen := byName[name]; seen {
			if prev.Optional && !n.Optional {
				prev.Optional = false
			}
			// A concrete config classification supersedes a weak ci/code/generic
			// one, so the badge reflects the strongest evidence.
			if _, prevWeak := weakKinds[prev.Kind]; prevWeak {
				if _, newWeak := weakKinds[kind]; !newWeak {
					prev.Kind = kind
				}
			}
			byName[name] = prev
			continue
		}
		byName[name] = SecretNeed{Name: name, Kind: kind, Optional: n.Optional}
	}
	if len(byName) == 0 {
		return nil
	}
	// Priority cap: credential-DECLARING secrets rank ahead of advisory code/CI
	// references, so source-grep noise can never evict a real credential.
	// Output re-sorted alpha for a stable ProfileHash.
	var strong, weak []SecretNeed
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		if s := byName[name]; s.Kind == "code" || s.Kind == "ci" {
			weak = append(weak, s)
		} else {
			strong = append(strong, s)
		}
	}
	out := strong
	if len(out) > maxSecretNeeds {
		out = out[:maxSecretNeeds]
	} else if room := maxSecretNeeds - len(out); room > 0 && len(weak) > 0 {
		if len(weak) > room {
			weak = weak[:room]
		}
		out = append(out, weak...)
	}
	slices.SortFunc(out, func(a, b SecretNeed) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// validateServices keeps only ids from the fixed service map, plus those
// implied by validated secret kinds; deduped/sorted/capped.
func validateServices(raw []string, needs []SecretNeed) []string {
	allowed := map[string]struct{}{}
	for _, v := range serviceByImagePrefix {
		allowed[v] = struct{}{}
	}
	set := map[string]struct{}{}
	for _, s := range raw {
		if _, ok := allowed[s]; ok {
			set[s] = struct{}{}
		}
	}
	for _, n := range needs {
		// Derive from the NAME, not the stored kind (which carries provenance,
		// e.g. deploy/code/ci) so MINIO_ROOT_PASSWORD still implies its service.
		if svc, ok := serviceForKind[classifySecretKind(n.Name)]; ok {
			set[svc] = struct{}{}
		}
	}
	out := gitremote.ToSorted(set)
	if len(out) > maxServices {
		out = out[:maxServices]
	}
	return out
}

// validateSuggestedHosts normalizes/validates hosts, drops ones already in
// the filename-keyed allowed set, and caps + sorts.
func validateSuggestedHosts(raw []string, allowedEgress map[string]struct{}) []string {
	set := map[string]struct{}{}
	for _, h := range raw {
		host := hostrules.HostOf(h) // normalize + validate; "" if invalid
		if host == "" {
			continue
		}
		if _, already := allowedEgress[host]; already {
			continue
		}
		set[host] = struct{}{}
	}
	out := gitremote.ToSorted(set)
	if len(out) > maxSuggested {
		out = out[:maxSuggested]
	}
	return out
}

// validateSecretFilePaths keeps pathSafe, bounded-length relative paths.
func validateSecretFilePaths(raw []string) []string {
	set := map[string]struct{}{}
	for _, p := range raw {
		if p == "" || len(p) > maxSecretPathLn || !pathSafe(p) || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			continue
		}
		set[p] = struct{}{}
	}
	out := gitremote.ToSorted(set)
	if len(out) > maxSecretFiles {
		out = out[:maxSecretFiles]
	}
	return out
}

// validateBuildMemoryMiB bounds an untrusted build heap to a sane ceiling
// (0 when unset/garbage).
func validateBuildMemoryMiB(v int) int {
	if v < 0 {
		return 0
	}
	if v > maxBuildMemMiB {
		return maxBuildMemMiB
	}
	return v
}

// validateLeakFindings keeps only known-kind findings with a safe relative
// path, dedupes, then sorts riskiest-first and caps (capLeakFindings). Never
// carries a value. truncated reports a cap drop, which the caller turns into
// NeedsReview rather than a silent drop.
func validateLeakFindings(raw []LeakFinding) (out []LeakFinding, truncated bool) {
	type key struct {
		path, kind string
		line       int
	}
	seen := map[key]struct{}{}
	for _, f := range raw {
		if _, ok := leakKinds[f.Kind]; !ok {
			continue
		}
		if f.Path == "" || len(f.Path) > maxSecretPathLn || !pathSafe(f.Path) || strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") {
			continue
		}
		line := f.Line
		if line < 0 {
			line = 0
		}
		k := key{f.Path, f.Kind, line}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, LeakFinding{Path: f.Path, Kind: f.Kind, Line: line})
	}
	return capLeakFindings(out)
}
