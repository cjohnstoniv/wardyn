// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// kataFetchSnippet is kataScript's download-and-unpack stage: from the
// release-assets marker up to the virtualization check, run under the
// script's own `set -euo pipefail`.
func kataFetchSnippet(t *testing.T) string {
	t.Helper()
	full := kataScript(linuxNative())
	_, rest, ok := strings.Cut(full, "# Kata's release assets")
	stage, _, ok2 := strings.Cut(rest, `echo "-> checking host virtualization"`)
	if !ok || !ok2 {
		t.Fatalf("could not extract the download stage from kataScript: %s", full)
	}
	return "set -euo pipefail\n# Kata's release assets" + stage
}

// runKataFetch runs the download stage with curl, sudo and zstd replaced by
// stubs that log their argv; curl exits curlRC without writing anything.
// It returns the logged curl and sudo argv lines and bash's error.
func runKataFetch(t *testing.T, curlRC string) (curl, sudo []string, err error) {
	t.Helper()
	if _, lerr := exec.LookPath("bash"); lerr != nil {
		t.Skip("bash not on PATH")
	}
	stubs, logs := t.TempDir(), t.TempDir()
	write := func(name, body string) {
		if werr := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body), 0o700); werr != nil {
			t.Fatal(werr)
		}
	}
	write("curl", `printf '%s\n' "$*" >>"$STUB_LOGS/curl"; exit "$CURL_RC"`+"\n")
	write("sudo", `printf '%s\n' "$*" >>"$STUB_LOGS/sudo"`+"\n")
	write("zstd", "exit 0\n")
	cmd := exec.Command("bash", "-c", kataFetchSnippet(t))
	cmd.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_LOGS="+logs, "CURL_RC="+curlRC, "VER=v3.31.0")
	err = cmd.Run()
	read := func(name string) []string {
		b, _ := os.ReadFile(filepath.Join(logs, name))
		return strings.Fields(strings.TrimSpace(string(b)))
	}
	return read("curl"), read("sudo"), err
}

// argAfter returns the argument following flag in argv, or "".
func argAfter(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// TestKataScript_ExtractsOnlyACompletedDownload: a failed download stops the
// script before any privileged step, and a completed one is unpacked from
// exactly the file curl wrote.
func TestKataScript_ExtractsOnlyACompletedDownload(t *testing.T) {
	t.Run("download fails", func(t *testing.T) {
		curl, sudo, err := runKataFetch(t, "22")
		if err == nil {
			t.Fatal("the stage succeeded after a failed download")
		}
		if len(curl) == 0 {
			t.Fatal("curl was never called; the stage did not reach the download")
		}
		if len(sudo) != 0 {
			t.Errorf("sudo ran after a failed download: %q", sudo)
		}
	})
	t.Run("download completes", func(t *testing.T) {
		curl, sudo, err := runKataFetch(t, "0")
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		out, in := argAfter(curl, "-o"), argAfter(sudo, "-xf")
		if out == "" || out != in {
			t.Errorf("curl wrote %q but tar read %q; want the same file", out, in)
		}
	})
}

// TestKataScript_StagesArchiveInPrivateTempDir: the archive is staged in a
// directory mktemp -d creates (owner-only) under /tmp, removed on exit, and
// never at a fixed name under the shared /tmp.
func TestKataScript_StagesArchiveInPrivateTempDir(t *testing.T) {
	script := kataScript(linuxNative())
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "/tmp/") && !strings.Contains(line, "mktemp -d /tmp/") {
			t.Errorf("fixed /tmp path outside mktemp: %q", line)
		}
	}
	mk := regexp.MustCompile(`(?m)^(\w+)="\$\(mktemp -d /tmp/[\w.-]*XXXXXX[\w]*\)"$`).FindStringSubmatch(script)
	if mk == nil {
		t.Fatalf("no `NAME=\"$(mktemp -d /tmp/...XXXXXX)\"` line:\n%s", script)
	}
	dir := `"$` + mk[1] + `/`
	order := []string{
		"set -euo pipefail",
		mk[0],
		`trap 'rm -rf "$` + mk[1] + `"' EXIT`,
		"curl -fsSL -o " + dir,
		"sudo tar -C / --zstd -xf " + dir,
	}
	at := -1
	for _, want := range order {
		i := strings.Index(script, want)
		if i < 0 {
			t.Fatalf("script lacks %q:\n%s", want, script)
		}
		if i <= at {
			t.Errorf("%q appears out of order", want)
		}
		at = i
	}

	curl, sudo, err := runKataFetch(t, "0")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	staged := argAfter(sudo, "-xf")
	if filepath.Dir(staged) == "/tmp" || !strings.HasPrefix(staged, "/tmp/") || argAfter(curl, "-o") != staged {
		t.Errorf("archive staged at %q; want a file in a fresh directory under /tmp", staged)
	}
	if _, serr := os.Stat(filepath.Dir(staged)); !os.IsNotExist(serr) {
		t.Errorf("staging directory %q still exists after the script exited (stat: %v)", filepath.Dir(staged), serr)
	}
}
