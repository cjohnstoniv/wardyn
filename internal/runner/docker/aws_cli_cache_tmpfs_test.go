// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The AWS CLI's derived role credentials (~/.aws/cli/cache) live on the
// sandbox's tmpfs, so a kept sandbox does not hold them past the run, and a real
// cache directory an earlier start left behind is deleted.
func TestAgentRunLib_AWSCLICacheLivesOnTmpfs(t *testing.T) {
	home, tmp := t.TempDir(), t.TempDir()
	stale := filepath.Join(home, ".aws", "cli", "cache")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "role.json"), []byte(`{"Credentials":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // the second start (a revive) finds its own link
		cmd := exec.Command("bash", "-c", `. "$1" && link_aws_cli_cache_to_tmpfs "$2"`, "bash", awsSSOAgentRunLibPath, tmp) //nolint:gosec // fixed in-repo script
		cmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("start %d: %v: %s", i, err, out)
		}
		got, err := os.Readlink(stale)
		if want := filepath.Join(tmp, "wardyn-aws-cli-cache"); err != nil || got != want {
			t.Fatalf("start %d: ~/.aws/cli/cache -> %q (%v), want a link to %s", i, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "wardyn-aws-cli-cache", "role.json")); !os.IsNotExist(err) {
		t.Fatalf("the earlier start's cached credentials survived: %v", err)
	}
	// materialize_aws_sso_config is what calls it, on every lane that writes ~/.aws.
	cmd := exec.Command("bash", "-c", `. "$1" && link_aws_cli_cache_to_tmpfs() { echo linked; } && WARDYN_AWS_SSO_CONFIG_B64=$'.aws/config\tW10K' materialize_aws_sso_config`, "bash", awsSSOAgentRunLibPath) //nolint:gosec // fixed in-repo script
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "linked\n" {
		t.Fatalf("materialize_aws_sso_config: %q (%v), want it to link the CLI cache", out, err)
	}
}
