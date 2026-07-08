// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"encoding/json"
	"strings"
	"testing"
)

func needNames(needs []SecretNeed) []string {
	if len(needs) == 0 {
		return nil
	}
	out := make([]string, 0, len(needs))
	for _, n := range needs {
		out = append(out, n.Name)
	}
	return out
}

func needByName(t *testing.T, needs []SecretNeed, name string) SecretNeed {
	t.Helper()
	for _, n := range needs {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("need %q not found in %v", name, needNames(needs))
	return SecretNeed{}
}

func TestScan_DotenvTemplateKeysOnly(t *testing.T) {
	dir := t.TempDir()
	// RHS-emptiness is the required/optional signal: uncommented KEY= (empty)
	// is a must-supply secret; uncommented KEY=<value> ships a default so it's
	// optional config; a commented line is an optional integration example.
	writeFile(t, dir, ".env.example",
		"# Optional integration\n"+
			"DATABASE_URL=postgres://user:pass@localhost/app\n"+ // has default ⇒ optional
			"export SESSION_SECRET=\n"+ // empty ⇒ required
			"POSTGRES_PASSWORD=\n"+ // empty ⇒ required
			"# STRIPE_SECRET_KEY=sk_test_abc\n"+ // commented ⇒ optional
			"not a key line\n")
	got := Scan(dir)
	eq(t, "RequiredSecrets names", needNames(got.RequiredSecrets),
		[]string{"DATABASE_URL", "POSTGRES_PASSWORD", "SESSION_SECRET", "STRIPE_SECRET_KEY"})
	if n := needByName(t, got.RequiredSecrets, "DATABASE_URL"); !n.Optional || n.Kind != "database" {
		t.Errorf("DATABASE_URL = %+v, want optional (has default)/database", n)
	}
	if n := needByName(t, got.RequiredSecrets, "SESSION_SECRET"); n.Optional || n.Kind != "session" {
		t.Errorf("SESSION_SECRET = %+v, want required (empty RHS)/session", n)
	}
	if n := needByName(t, got.RequiredSecrets, "POSTGRES_PASSWORD"); n.Optional || n.Kind != "postgres" {
		t.Errorf("POSTGRES_PASSWORD = %+v, want required (empty RHS)/postgres", n)
	}
	if n := needByName(t, got.RequiredSecrets, "STRIPE_SECRET_KEY"); !n.Optional || n.Kind != "stripe" {
		t.Errorf("STRIPE_SECRET_KEY = %+v, want optional (commented)/stripe", n)
	}
	// An empty POSTGRES_PASSWORD= implies the postgres service (family classify).
	eq(t, "ServicesNeeded", got.ServicesNeeded, []string{"postgres"})
	if got.Confidence != ConfidenceHigh {
		t.Errorf("Confidence = %v, want high (content lane never lowers it)", got.Confidence)
	}
}

func TestScan_RealEnvIsPresenceOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env", "REAL_KEY=hunter2-actual-value\n")
	writeFile(t, dir, "backend/.env.local", "OTHER=alsoreal\n")
	writeFile(t, dir, ".envrc", "export FOO=bar\n") // direnv marker, NOT a dotenv secret file
	got := Scan(dir)
	eq(t, "SecretFilesPresent", got.SecretFilesPresent, []string{".env", "backend/.env.local"})
	// Presence only: the keys inside a real .env are never extracted.
	eq(t, "RequiredSecrets", needNames(got.RequiredSecrets), nil)
	eq(t, "Tools", got.Tools, []string{"direnv"})
}

func TestScan_SpringPlaceholders(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "application.properties",
		"spring.datasource.url=jdbc:postgresql://localhost/app\n"+
			"spring.datasource.password=${POSTGRES_PASSWORD}\n"+
			"server.port=${SERVER_PORT:8080}\n"+
			"some.literal=реальное-значение\n")
	got := Scan(dir)
	eq(t, "RequiredSecrets names", needNames(got.RequiredSecrets), []string{"POSTGRES_PASSWORD", "SERVER_PORT"})
	if n := needByName(t, got.RequiredSecrets, "POSTGRES_PASSWORD"); n.Optional {
		t.Errorf("no-default placeholder must be required: %+v", n)
	}
	if n := needByName(t, got.RequiredSecrets, "SERVER_PORT"); !n.Optional {
		t.Errorf("defaulted placeholder must be optional: %+v", n)
	}
	// POSTGRES_* implies the service even without a compose file.
	eq(t, "ServicesNeeded", got.ServicesNeeded, []string{"postgres"})
}

func TestScan_ComposeServicesAndInterpolation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "docker-compose.yml",
		"services:\n"+
			"  db:\n"+
			"    image: postgres:16\n"+
			"    environment:\n"+
			"      POSTGRES_PASSWORD: ${DB_PASSWORD?Variable not set}\n"+
			"  cache:\n"+
			"    image: \"redis:7-alpine\"\n"+
			"  store:\n"+
			"    image: quay.io/minio/minio:latest\n"+
			"  app:\n"+
			"    build: .\n"+
			"    environment:\n"+
			"      - DEBUG=${DEBUG:-0}\n")
	got := Scan(dir)
	eq(t, "ServicesNeeded", got.ServicesNeeded, []string{"minio", "postgres", "redis"})
	if n := needByName(t, got.RequiredSecrets, "DB_PASSWORD"); n.Optional {
		t.Errorf("${VAR?err} must be required: %+v", n)
	}
	if n := needByName(t, got.RequiredSecrets, "DEBUG"); !n.Optional {
		t.Errorf("${VAR:-def} must be optional: %+v", n)
	}
}

func TestScan_SealedSecretKeys(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "overlays/prod/sealedsecret-db.yaml",
		"apiVersion: bitnami.com/v1alpha1\n"+
			"kind: SealedSecret\n"+
			"metadata:\n"+
			"  name: app-db\n"+
			"spec:\n"+
			"  encryptedData:\n"+
			"    DATABASE_URL: AgBhx8VeryLongCiphertextBlobZZZ==\n"+
			"    credentials.json: AgCanotherCipherBlobYYY==\n"+
			"  template:\n"+
			"    metadata:\n"+
			"      name: app-db\n")
	got := Scan(dir)
	eq(t, "RequiredSecrets names", needNames(got.RequiredSecrets), []string{"DATABASE_URL", "credentials.json"})
	for _, name := range []string{"DATABASE_URL", "credentials.json"} {
		if n := needByName(t, got.RequiredSecrets, name); !n.Optional || n.Kind != "deploy" {
			t.Errorf("%s = %+v, want deploy-time/optional", name, n)
		}
	}
	// Keys outside the encryptedData block (template.metadata.name) must not leak in.
	if len(got.RequiredSecrets) != 2 {
		t.Errorf("captured keys outside encryptedData: %v", needNames(got.RequiredSecrets))
	}
}

func TestScan_DockerfileFromIsSuggestedNeverAllowed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile",
		"FROM ghcr.io/acme/base:1 AS build\n"+
			"FROM build\n"+ // stage alias — not a registry
			"FROM scratch\n"+
			"FROM node:20-alpine\n"+
			"FROM registry.example.com:5000/tools/thing:2\n")
	got := Scan(dir)
	eq(t, "SuggestedEgress", got.SuggestedEgress, []string{"docker.io", "ghcr.io", "registry.example.com"})
	// The load-bearing half: content-derived hosts must NOT be auto-allowed.
	eq(t, "EgressDomains", got.EgressDomains, nil)
}

func TestScan_WrapperPropertiesAreStaticEgress(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "gradle/wrapper/gradle-wrapper.properties",
		"distributionUrl=https\\://services.gradle.org/distributions/gradle-8.7-bin.zip\n")
	writeFile(t, dir, ".mvn/wrapper/maven-wrapper.properties",
		"distributionUrl=https://repo.maven.apache.org/maven2/org/apache/maven/apache-maven/3.9.6/apache-maven-3.9.6-bin.zip\n")
	got := Scan(dir)
	// Filename-keyed lane: these MAY auto-union (fixed hosts from the table,
	// never from the file's content).
	eq(t, "EgressDomains", got.EgressDomains,
		[]string{"repo.maven.apache.org", "repo1.maven.org", "services.gradle.org"})
	eq(t, "Tools", got.Tools, []string{"gradle-wrapper", "maven-wrapper"})
}

// TestScan_ValuesNeverLeak is the load-bearing security regression: secret
// VALUES planted in every detector-target file must appear nowhere in the
// emitted facts OR the derived profile.
func TestScan_ValuesNeverLeak(t *testing.T) {
	dir := t.TempDir()
	const (
		v1 = "supersecretvalue123"
		v2 = "topsecret999realdotenv"
		v3 = "sk-live-veryrealapikey456"
	)
	writeFile(t, dir, ".env.example", "SECRET="+v1+"\n")
	writeFile(t, dir, ".env", "REAL="+v2+"\n")
	writeFile(t, dir, "application.yml", "api:\n  key: "+v3+"\n  other: ${API_KEY}\n")
	writeFile(t, dir, "docker-compose.yml", "services:\n  db:\n    image: postgres:16\n    environment:\n      PW: "+v1+"\n")

	facts := CollectFacts(dir)
	profile := DeriveProfile(facts)
	for label, obj := range map[string]any{"facts": facts, "profile": profile} {
		b, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{v1, v2, v3} {
			if strings.Contains(string(b), v) {
				t.Errorf("%s JSON contains planted secret value %q", label, v)
			}
		}
	}
	// None of the detector targets may reach the LLM-bound sample set.
	if len(facts.UnrecognizedSamples) != 0 {
		t.Errorf("detector-target files leaked into UnrecognizedSamples: %+v", facts.UnrecognizedSamples)
	}
	eq(t, "SecretFilesPresent", profile.SecretFilesPresent, []string{".env"})
	eq(t, "RequiredSecrets names", needNames(profile.RequiredSecrets), []string{"API_KEY", "SECRET"})
}

// TestDeriveProfile_HostileNeedsFactsCapped: DeriveProfile must survive and
// neutralize a hostile facts upload (the repo-scan path is sandbox-controlled).
func TestDeriveProfile_HostileNeedsFactsCapped(t *testing.T) {
	var facts ScanFacts
	for i := 0; i < 10000; i++ {
		facts.SecretRequirements = append(facts.SecretRequirements,
			SecretNeed{Name: "KEY_" + strings.Repeat("A", i%3) + string(rune('A'+i%26)) + itoa(i), Kind: "not-a-kind"})
	}
	facts.SecretRequirements = append(facts.SecretRequirements,
		SecretNeed{Name: "bad name with spaces"},
		SecretNeed{Name: "ansi\x1b[31mred"},
		SecretNeed{Name: strings.Repeat("X", 500)},
		SecretNeed{Name: "ünïcode"},
		SecretNeed{Name: ""},
	)
	facts.ServicesFound = []string{"postgres", "evil-service", "postgres", "redis"}
	facts.SuggestedEgress = []string{
		"https://evil.example.com/path", "EVIL.EXAMPLE.COM:8443", "bareword",
		"ok.example.org", strings.Repeat("a", 300) + ".com", "-bad.example.com",
	}
	facts.SecretFilesPresent = []string{"../../etc/passwd", "/abs/path", "ok/.env", "bad\x01path"}

	got := DeriveProfile(facts)
	if len(got.RequiredSecrets) != maxSecretNeeds {
		t.Errorf("RequiredSecrets len = %d, want cap %d", len(got.RequiredSecrets), maxSecretNeeds)
	}
	for _, n := range got.RequiredSecrets {
		if !needNameRE.MatchString(n.Name) {
			t.Errorf("non-conforming name survived: %q", n.Name)
		}
		if n.Kind != "generic" {
			t.Errorf("unknown kind not coerced: %+v", n)
		}
	}
	eq(t, "ServicesNeeded", got.ServicesNeeded, []string{"postgres", "redis"})
	eq(t, "SuggestedEgress", got.SuggestedEgress, []string{"evil.example.com", "ok.example.org"})
	eq(t, "SecretFilesPresent", got.SecretFilesPresent, []string{"ok/.env"})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestScan_RequiredWinsOverOptionalDuplicate: the same key declared optional
// in one file and required in another must surface as required.
func TestScan_RequiredWinsOverOptionalDuplicate(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env.example", "# API_KEY=commented-optional\n")
	writeFile(t, dir, "application.properties", "api.key=${API_KEY}\n")
	got := Scan(dir)
	if n := needByName(t, got.RequiredSecrets, "API_KEY"); n.Optional {
		t.Errorf("required declaration must beat optional duplicate: %+v", n)
	}
}

// ── deferral closures: new detectors ────────────────────────────────────────

func TestScan_EnvAccessFromSourceIsAdvisory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/Main.java", "String k = System.getenv(\"DATAGOLF_API_KEY\");\n")
	writeFile(t, dir, "web/app.ts", "const u = process.env.STRIPE_SECRET_KEY;\nconst v = import.meta.env.VITE_BASE;\n")
	writeFile(t, dir, "svc/handler.py", "tok = os.environ[\"AGENT_TOKEN\"]\nx = os.getenv('REDIS_URL')\ny = os.environ.get(\"KG_ADMIN_TOKEN\")\n")
	got := Scan(dir)
	// Code refs are advisory (optional) — they enrich the panel, never the
	// required checklist.
	for _, name := range []string{"DATAGOLF_API_KEY", "STRIPE_SECRET_KEY", "VITE_BASE", "AGENT_TOKEN", "REDIS_URL", "KG_ADMIN_TOKEN"} {
		n := needByName(t, got.RequiredSecrets, name)
		if !n.Optional {
			t.Errorf("%s from source must be optional/advisory: %+v", name, n)
		}
	}
}

func TestScan_CISecretsSuppressedAsOptional(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".github/workflows/ci.yml",
		"jobs:\n  build:\n    steps:\n"+
			"      - env:\n"+
			"          AWS: ${{ secrets.AWS_SECRET_ACCESS_KEY }}\n"+
			"          TOK: ${{ secrets.GITHUB_TOKEN }}\n")
	got := Scan(dir)
	// GITHUB_TOKEN is auto-provided — never surfaced.
	for _, n := range got.RequiredSecrets {
		if n.Name == "GITHUB_TOKEN" {
			t.Error("GITHUB_TOKEN must not be surfaced (Actions auto-provides it)")
		}
	}
	n := needByName(t, got.RequiredSecrets, "AWS_SECRET_ACCESS_KEY")
	if !n.Optional || n.Kind != "ci" {
		t.Errorf("CI-only secret = %+v, want optional/ci (suppressed from checklist)", n)
	}
}

// A name in BOTH a required .env and CI keeps the stronger (required) status.
func TestScan_CISecretDefersToRealDeclaration(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".env.example", "AWS_SECRET_ACCESS_KEY=\n") // empty ⇒ required
	writeFile(t, dir, ".github/workflows/ci.yml", "x: ${{ secrets.AWS_SECRET_ACCESS_KEY }}\n")
	got := Scan(dir)
	n := needByName(t, got.RequiredSecrets, "AWS_SECRET_ACCESS_KEY")
	if n.Optional || n.Kind == "ci" {
		t.Errorf("a real required declaration must beat a CI sighting: %+v", n)
	}
}

func TestScan_SecretKeyRefBlockAndInline(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "base/deployment.yaml",
		"spec:\n  containers:\n    - env:\n"+
			"        - name: TOKEN\n"+
			"          valueFrom:\n"+
			"            secretKeyRef:\n"+
			"              name: app-secrets\n"+
			"              key: MLBHR_BATCH_TOKEN\n"+
			"        - name: ROOT\n"+
			"          valueFrom: { secretKeyRef: { name: minio-root, key: MINIO_ROOT_PASSWORD } }\n")
	got := Scan(dir)
	for _, name := range []string{"MLBHR_BATCH_TOKEN", "MINIO_ROOT_PASSWORD"} {
		n := needByName(t, got.RequiredSecrets, name)
		if !n.Optional || n.Kind != "deploy" {
			t.Errorf("%s = %+v, want deploy/optional", name, n)
		}
	}
	// The k8s Secret's own name (app-secrets/minio-root) must NOT be captured.
	for _, n := range got.RequiredSecrets {
		if n.Name == "app-secrets" || n.Name == "minio-root" {
			t.Errorf("captured the Secret name, not a data key: %s", n.Name)
		}
	}
}

func TestScan_MavenReposScopedToRepositoryBlock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pom.xml",
		"<project>\n"+
			"  <url>https://project-home.example.com</url>\n"+ // project URL — must NOT leak
			"  <licenses><license><url>https://www.apache.org/licenses/LICENSE-2.0</url></license></licenses>\n"+
			"  <repositories>\n"+
			"    <repository><id>central</id><url>https://repo.mycorp.example.com/maven</url></repository>\n"+
			"  </repositories>\n"+
			"</project>\n")
	got := Scan(dir)
	eq(t, "SuggestedEgress", got.SuggestedEgress, []string{"repo.mycorp.example.com"})
}

func TestScan_GradleReposSuggested(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "build.gradle",
		"repositories {\n  mavenCentral()\n  maven { url \"https://jitpack.example.io\" }\n}\n")
	got := Scan(dir)
	eq(t, "SuggestedEgress", got.SuggestedEgress, []string{"jitpack.example.io"})
}

func TestScan_BuildHeapDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "gradle.properties", "org.gradle.jvmargs=-Xmx4g -XX:MaxMetaspaceSize=1G\n")
	writeFile(t, dir, "Dockerfile", "FROM node:20\nENV NODE_OPTIONS=--max-old-space-size=8192\n")
	got := Scan(dir)
	if got.BuildMemoryMiB != 8192 { // max(4096, 8192)
		t.Errorf("BuildMemoryMiB = %d, want 8192 (max of 4g Xmx and 8192 node)", got.BuildMemoryMiB)
	}
}

func TestScan_LeakFindingsAreContentFree(t *testing.T) {
	dir := t.TempDir()
	// A real-shaped AWS key + GitHub token committed in tracked source.
	writeFile(t, dir, "config/prod.yaml",
		"aws_key: AKIAIOSFODNN7EXAMPLE\ntoken: ghp_012345678901234567890123456789ABCDeF\n")
	got := Scan(dir)
	kinds := map[string]bool{}
	for _, f := range got.LeakFindings {
		kinds[f.Kind] = true
		if f.Line == 0 {
			t.Errorf("leak finding missing line: %+v", f)
		}
	}
	if !kinds["aws-access-key"] || !kinds["github-token"] {
		t.Errorf("expected aws + github leak findings, got %+v", got.LeakFindings)
	}
	// The matched VALUES must appear nowhere in the profile.
	b, _ := json.Marshal(got)
	for _, v := range []string{"AKIAIOSFODNN7EXAMPLE", "ghp_012345678901234567890123456789ABCDeF"} {
		if strings.Contains(string(b), v) {
			t.Errorf("leak finding leaked the value %q into the profile", v)
		}
	}
}

func TestDeriveProfile_HostileNewFieldsCapped(t *testing.T) {
	var facts ScanFacts
	facts.BuildMemoryMiB = 1 << 30 // absurd
	for i := 0; i < 500; i++ {
		facts.LeakFindings = append(facts.LeakFindings, LeakFinding{Path: "a.go", Kind: "not-a-real-kind", Line: i})
	}
	facts.LeakFindings = append(facts.LeakFindings,
		LeakFinding{Path: "../etc/x", Kind: "aws-access-key", Line: 1}, // bad path
		LeakFinding{Path: "ok.go", Kind: "github-token", Line: 3},
	)
	got := DeriveProfile(facts)
	if got.BuildMemoryMiB != maxBuildMemMiB {
		t.Errorf("BuildMemoryMiB = %d, want capped %d", got.BuildMemoryMiB, maxBuildMemMiB)
	}
	if len(got.LeakFindings) != 1 || got.LeakFindings[0].Kind != "github-token" {
		t.Errorf("LeakFindings = %+v, want only the one valid finding", got.LeakFindings)
	}
}

// ── setup-command detection (fixed templates, never file content) ────────────

func cmdFor(cmds []SetupCommand, stage string) []string {
	var out []string
	for _, c := range cmds {
		if c.Stage == stage {
			out = append(out, c.Command)
		}
	}
	return out
}

func TestScan_SetupCommandsFromTemplates(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name":"x","scripts":{"build":"tsc","test":"vitest","lint":"eslint .","postinstall":"echo evil"}}`)
	writeFile(t, dir, "pnpm-lock.yaml", "lockfileVersion: '6.0'\n")
	got := Scan(dir)
	// Install command is the FIXED pnpm template, not any script body.
	eq(t, "install", cmdFor(got.SetupCommands, "install"), []string{"pnpm install --frozen-lockfile"})
	eq(t, "build", cmdFor(got.SetupCommands, "build"), []string{"pnpm run build"})
	eq(t, "test", cmdFor(got.SetupCommands, "test"), []string{"pnpm run test"})
	eq(t, "lint", cmdFor(got.SetupCommands, "lint"), []string{"pnpm run lint"})
	// The malicious scripts.postinstall body must appear NOWHERE.
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "echo evil") || strings.Contains(string(b), "postinstall") {
		t.Errorf("script body leaked into profile: %s", b)
	}
}

func TestScan_SetupCommandsGoAndMavenWrapper(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module x\n\ngo 1.22\n")
	got := Scan(dir)
	eq(t, "go install", cmdFor(got.SetupCommands, "install"), []string{"go mod download"})
	eq(t, "go build", cmdFor(got.SetupCommands, "build"), []string{"go build ./..."})
	eq(t, "go test", cmdFor(got.SetupCommands, "test"), []string{"go test ./..."})

	dir2 := t.TempDir()
	writeFile(t, dir2, "pom.xml", "<project></project>\n")
	writeFile(t, dir2, ".mvn/wrapper/maven-wrapper.properties", "distributionUrl=https://repo.maven.apache.org/x.zip\n")
	got2 := Scan(dir2)
	// Wrapper present ⇒ ./mvnw, not mvn.
	build := cmdFor(got2.SetupCommands, "build")
	if len(build) != 1 || !strings.HasPrefix(build[0], "./mvnw ") {
		t.Errorf("maven build = %v, want ./mvnw ...", build)
	}
}

func TestScan_SetupCommandsMakefileFallback(t *testing.T) {
	dir := t.TempDir()
	// A repo with a Makefile but no recognized package manager: make targets fill in.
	writeFile(t, dir, "Makefile", "build:\n\techo build\ntest:\n\techo test\n.PHONY: build test\n")
	got := Scan(dir)
	eq(t, "make build", cmdFor(got.SetupCommands, "build"), []string{"make build"})
	eq(t, "make test", cmdFor(got.SetupCommands, "test"), []string{"make test"})
}

func TestDeriveProfile_HostileScriptKeysIgnored(t *testing.T) {
	// A hostile facts upload can put anything in ScriptKeys; only build/test/lint
	// ever produce a (fixed-template) command.
	facts := ScanFacts{
		ManifestsFound: []ManifestHit{{Path: "package-lock.json", Marker: "package-lock.json"}},
		ScriptKeys:     []string{"build", "evil; rm -rf /", "$(curl evil)"},
	}
	got := DeriveProfile(facts)
	for _, c := range got.SetupCommands {
		if strings.ContainsAny(c.Command, ";$|&`") {
			t.Errorf("hostile script key produced a shell-metachar command: %q", c.Command)
		}
	}
	if cmds := cmdFor(got.SetupCommands, "build"); len(cmds) != 1 || cmds[0] != "npm run build" {
		t.Errorf("build = %v, want [npm run build]", cmds)
	}
}

func TestScan_ContextHashBustsOnBuildInputChange(t *testing.T) {
	mk := func(dockerfile string) WorkspaceProfile {
		dir := t.TempDir()
		writeFile(t, dir, "go.mod", "module x\n\ngo 1.22\n")
		writeFile(t, dir, "Dockerfile", dockerfile)
		return Scan(dir)
	}
	a := mk("FROM golang:1.22\n")
	b := mk("FROM golang:1.23\n") // same profile (Go), different build input
	if a.ContextHash == "" {
		t.Fatal("ContextHash should be set when a Dockerfile is present")
	}
	if a.ContextHash == b.ContextHash {
		t.Error("a Dockerfile content change must bust ContextHash")
	}
	if a.ProfileHash() == b.ProfileHash() {
		t.Error("ProfileHash (image cache key) must bust when the build input changes")
	}
	// No build input ⇒ empty ContextHash (generated-devcontainer path is profile-derived).
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module y\n\ngo 1.22\n")
	if Scan(dir).ContextHash != "" {
		t.Error("no build-input files ⇒ empty ContextHash")
	}
}
