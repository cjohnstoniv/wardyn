// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cliutil.ScrubChildEnv calls itself "the one env denylist shared by every
// host-exec'd third-party CLI child" (internal/cliutil/cliutil.go's package
// doc). It was not: NO exec site in cmd/wardyn assigned it, so `wardyn ssh`
// handed ssh(1) — a third-party binary that runs the operator's own
// ProxyCommand/LocalCommand children and can SendEnv to a remote host — the
// live WARDYN_ADMIN_TOKEN, the WARDYN_AGE_KEY secret-store master key and
// ANTHROPIC_API_KEY, and the setup installer handed the same to a sudo'd
// `bash -c`.
//
// This is a SOURCE guard rather than a behavioural one because the leak is a
// missing line, not a wrong value: the only way to prove it stays fixed is to
// require the line at every exec site, including ones written later.

// execSiteRe matches `x := exec.Command(...)` / `exec.CommandContext(...)`,
// capturing the variable an Env assignment would have to name.
var execSiteRe = regexp.MustCompile(`(\w+)\s*:?=\s*exec\.Command(?:Context)?\(`)

// scrubbedEnvExceptions names the exec sites that deliberately inherit the
// full environment, with the reason. gatherComposeConfig exists to resolve
// ${WARDYN_*} against the REAL environment — that IS its output — and what it
// returns is passed through redactSecrets before it reaches a bundle.
var scrubbedEnvExceptions = map[string]string{
	"supportbundle.go": "gatherComposeConfig resolves ${WARDYN_*} on purpose; its output is redacted by redactSecrets",
}

func TestEveryHostExecChildGetsTheScrubbedEnv(t *testing.T) {
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	checked := 0
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if !strings.Contains(line, "exec.Command") {
				continue
			}
			if why, ok := scrubbedEnvExceptions[path]; ok {
				t.Logf("%s:%d exempt — %s", path, i+1, why)
				continue
			}
			checked++
			// The inline form (`out, err := exec.Command(...).Output()`) has no
			// *exec.Cmd to assign Env to at all — it must be split first.
			inline := strings.Contains(line, ").Output()") ||
				strings.Contains(line, ").Run()") ||
				strings.Contains(line, ").CombinedOutput()")
			m := execSiteRe.FindStringSubmatch(line)
			if inline || m == nil {
				t.Errorf("%s:%d execs a child inline (no variable), so it cannot assign Env:\n\t%s",
					path, i+1, strings.TrimSpace(line))
				continue
			}
			want := m[1] + ".Env = cliutil.ScrubChildEnv(os.Environ())"
			found := false
			for j := i + 1; j < len(lines) && j <= i+6; j++ {
				if strings.Contains(lines[j], want) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s:%d execs a third-party CLI without the shared env denylist; add\n\t%s\n\timmediately after:\n\t%s",
					path, i+1, want, strings.TrimSpace(line))
			}
		}
	}
	if checked == 0 {
		t.Fatal("no exec sites found in cmd/wardyn — the guard is scanning nothing")
	}
}
