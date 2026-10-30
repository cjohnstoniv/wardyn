// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestAggregateImageCheck(t *testing.T) {
	v := func(arch, code, detail string) ImageArchVerdict {
		return ImageArchVerdict{Arch: arch, Code: code, Detail: detail}
	}
	for _, c := range []struct {
		name     string
		in       []ImageArchVerdict
		status   string
		reason   string
		accepted []string
	}{
		{"both pass", []ImageArchVerdict{v("amd64", ImageCheckPassed, ""), v("arm64", ImageCheckPassed, "")},
			types.BaseImageCheckReady, "", []string{"amd64", "arm64"}},
		{"one platform missing is still ready", []ImageArchVerdict{v("amd64", ImageCheckPassed, ""), v("arm64", ImageCheckPlatformMissing, "")},
			types.BaseImageCheckReady, "platform_missing:arm64", []string{"amd64"}},
		{"no node of one arch is still ready", []ImageArchVerdict{v("amd64", ImageCheckNoNode, ""), v("arm64", ImageCheckPassed, "")},
			types.BaseImageCheckReady, "no_node:amd64", []string{"arm64"}},
		{"a contract failure anywhere refuses", []ImageArchVerdict{v("amd64", ImageCheckPassed, ""), v("arm64", ImageCheckContractMissing, "wardyn-rec")},
			types.BaseImageCheckRefused, "contract_missing:arm64:wardyn-rec", nil},
		{"unpullable refuses", []ImageArchVerdict{v("amd64", ImageCheckUnpullable, "unauthorized"), v("arm64", ImageCheckNoNode, "")},
			types.BaseImageCheckRefused, "unpullable:amd64:unauthorized", nil},
		{"timed out refuses", []ImageArchVerdict{v("amd64", ImageCheckPassed, ""), v("arm64", ImageCheckTimedOut, "")},
			types.BaseImageCheckRefused, "timed_out:arm64", nil},
		{"nothing passed, only left out", []ImageArchVerdict{v("amd64", ImageCheckNoNode, ""), v("arm64", ImageCheckPlatformMissing, "")},
			types.BaseImageCheckRefused, "no_node:amd64", nil},
		{"no verdicts at all", nil, types.BaseImageCheckRefused, "failed::no architecture was checked", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, reason, accepted := AggregateImageCheck(c.in)
			if status != c.status || reason != c.reason || !slices.Equal(accepted, c.accepted) {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", status, reason, accepted, c.status, c.reason, c.accepted)
			}
		})
	}
}

// TestImageCheckScript runs the check pod's command against a stand-in /usr/local/bin: the stock contract
// passes, a missing tool exits 3 naming it, and a wardyn-rec without -contract or with another line exits 4.
func TestImageCheckScript(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	const goodRec = "#!/bin/sh\n[ \"$1\" = -contract ] && echo '" + types.ImageContractLine + "' && exit 0\nexit 1\n"
	setup := func(t *testing.T, rec string, skip string) string {
		dir := t.TempDir()
		for _, tool := range types.ImageContractTools {
			if tool == skip {
				continue
			}
			body := "#!/bin/sh\nexit 0\n"
			if tool == "wardyn-rec" {
				body = rec
			}
			if err := os.WriteFile(filepath.Join(dir, tool), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	run := func(dir string) (int, string) {
		script := strings.ReplaceAll(ImageCheckScript, "/usr/local/bin/", dir+"/")
		out, err := exec.Command(sh, "-c", script).CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), strings.TrimSpace(string(out))
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0, strings.TrimSpace(string(out))
	}
	if code, out := run(setup(t, goodRec, "")); code != 0 || out != types.ImageContractLine {
		t.Fatalf("stock contract: exit %d %q, want 0 and the contract line", code, out)
	}
	for _, tool := range types.ImageContractTools {
		if code, out := run(setup(t, goodRec, tool)); code != 3 || out != "missing "+tool {
			t.Fatalf("without %s: exit %d %q, want 3 naming it", tool, code, out)
		}
	}
	if code, _ := run(setup(t, "#!/bin/sh\nexit 1\n", "")); code != 4 {
		t.Fatalf("wardyn-rec without -contract: exit %d, want 4", code)
	}
	if code, _ := run(setup(t, "#!/bin/sh\necho 'wardyn image contract 2'\n", "")); code != 4 {
		t.Fatalf("wardyn-rec printing another line: exit %d, want 4", code)
	}
}
