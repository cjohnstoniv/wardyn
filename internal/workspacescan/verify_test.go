// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeriveVerifyResult_RecomputesOKAndMasks(t *testing.T) {
	// Uploader claims OK=true, but a step failed and a log printed a secret-shaped
	// token. DeriveVerifyResult must recompute OK=false and mask the token.
	raw := VerifyResult{
		OK: true, // lie
		Steps: []VerifyStepResult{
			{Stage: "install", Command: "npm ci", ExitCode: 0, LogHead: "ok"},
			{Stage: "build", Command: "npm run build", ExitCode: 1,
				LogTail: "leaked AKIAIOSFODNN7EXAMPLE and ghp_012345678901234567890123456789ABCDeF"},
			{Stage: "evil", Command: "x", ExitCode: 0}, // unknown stage → "run"
		},
	}
	got := DeriveVerifyResult(raw)
	if got.OK {
		t.Error("OK must be recomputed false (a step exited non-zero)")
	}
	if !got.Ran {
		t.Error("Ran must be true")
	}
	if got.Steps[2].Stage != "run" {
		t.Errorf("unknown stage should coerce to 'run', got %q", got.Steps[2].Stage)
	}
	b, _ := json.Marshal(got)
	for _, secret := range []string{"AKIAIOSFODNN7EXAMPLE", "ghp_012345678901234567890123456789ABCDeF"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("secret-shaped token %q leaked into verify result (should be masked)", secret)
		}
	}
	if !strings.Contains(string(b), "masked") {
		t.Error("expected a masked placeholder in the logs")
	}
}

func TestDeriveVerifyResult_CapsAndExitClamp(t *testing.T) {
	var raw VerifyResult
	for i := 0; i < 100; i++ {
		raw.Steps = append(raw.Steps, VerifyStepResult{Stage: "test", Command: "x", ExitCode: 9999})
	}
	got := DeriveVerifyResult(raw)
	if len(got.Steps) != maxVerifySteps {
		t.Errorf("steps = %d, want cap %d", len(got.Steps), maxVerifySteps)
	}
	if got.Steps[0].ExitCode != 255 {
		t.Errorf("exit clamp: got %d want 255", got.Steps[0].ExitCode)
	}
}

func TestDeriveVerifyResult_EmptyIsNotRan(t *testing.T) {
	got := DeriveVerifyResult(VerifyResult{})
	if got.Ran || got.OK {
		t.Errorf("empty verify result: Ran=%v OK=%v, want false/false", got.Ran, got.OK)
	}
}

func TestClassifyFailureHint(t *testing.T) {
	cases := []struct {
		name     string
		command  string
		exitCode int
		logTail  string
		want     string // substring expected in the hint, "" means no hint
	}{
		{
			name: "exit 127 toolchain missing", command: "go build ./...", exitCode: 127,
			logTail: "sh: 1: go: not found", want: "toolchain isn't in the sandbox image",
		},
		{
			name: "command not found text even with a different exit code", command: "cargo test", exitCode: 1,
			logTail: "bash: cargo: command not found", want: "toolchain isn't in the sandbox image",
		},
		{
			name: "maven unknown host", command: "mvn test", exitCode: 1,
			logTail: "Unknown host repo.maven.apache.org: nodename nor servname provided", want: "Maven proxy",
		},
		{
			name: "maven could not transfer", command: "mvn install", exitCode: 1,
			logTail: "Could not transfer artifact org.foo:bar:pom:1.0", want: "Maven proxy",
		},
		{
			name: "go test permission denied on /tmp", command: "go test ./...", exitCode: 1,
			logTail: "/tmp/go-buildXYZ/b001/b001.test: permission denied", want: "noexec",
		},
		{
			name: "no matching signature", command: "npm test", exitCode: 1,
			logTail: "1 test failed: expected true, got false", want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyFailureHint(c.command, c.exitCode, c.logTail)
			if c.want == "" {
				if got != "" {
					t.Errorf("expected no hint, got %q", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("hint %q does not contain %q", got, c.want)
			}
		})
	}
}

func TestDeriveVerifyResult_AttachesFailureHintFromFirstFailingStep(t *testing.T) {
	raw := VerifyResult{
		Steps: []VerifyStepResult{
			{Stage: "install", Command: "npm ci", ExitCode: 0},
			{Stage: "build", Command: "mvnw build", ExitCode: 127, LogTail: "sh: 1: mvnw: not found"},
			{Stage: "test", Command: "npm test", ExitCode: 1, LogTail: "unrelated assertion failure"},
		},
	}
	got := DeriveVerifyResult(raw)
	if !strings.Contains(got.FailureHint, "toolchain isn't in the sandbox image") {
		t.Errorf("expected the FIRST failing step's hint, got %q", got.FailureHint)
	}
}

func TestMaskSecretShaped_MultiLinePEM(t *testing.T) {
	log := "setting up\n-----BEGIN RSA PRIVATE KEY-----\nMIIEabc123secretkeymaterial\nmoresecretbytes==\n-----END RSA PRIVATE KEY-----\ndone"
	masked := MaskSecretShaped(log)
	if strings.Contains(masked, "MIIEabc123secretkeymaterial") || strings.Contains(masked, "moresecretbytes") {
		t.Errorf("multi-line PEM body not masked: %q", masked)
	}
	if !strings.Contains(masked, "private-key-masked") {
		t.Errorf("expected mask placeholder, got %q", masked)
	}
	gcp := `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n"}`
	m2 := MaskSecretShaped(gcp)
	if strings.Contains(m2, "abc\\n") || strings.Contains(m2, "BEGIN PRIVATE KEY") {
		t.Errorf("gcp private_key not masked: %q", m2)
	}
}
