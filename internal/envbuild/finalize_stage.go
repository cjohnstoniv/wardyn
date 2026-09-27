// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

// The BYOI wrap's second stage: the generated Dockerfile, the system
// gitconfig it installs, and the in-memory tar build context both ride in —
// what the wrap ADDS to an operator's base image, and why each piece is a
// COPY rather than a RUN.

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// finalizeDockerfile is the trusted second-stage Dockerfile. It clears
// ENTRYPOINT so the runner's Cmd (sleep infinity / agent-run) runs directly
// — an inherited ENTRYPOINT would wrap it and tear the sandbox down
// immediately. The tools land 0755 on PATH via the tar entry mode, so no RUN
// chmod (and no shell in the base image) is required.
//
// The system gitconfig is COPYed, never `RUN git config --system`: a BYOI
// base is not guaranteed to carry a shell OR git. COPY keeps this stage
// "FROM + COPY only", which is the property assertWrapSafeBase depends on.
func finalizeDockerfile(baseRef string) string {
	return "FROM " + baseRef + "\n" +
		"COPY tools/ /usr/local/bin/\n" +
		"COPY gitconfig /etc/gitconfig\n" +
		"ENTRYPOINT []\n" +
		"CMD []\n"
}

// finalizeGitConfig is the system gitconfig the wrap lays down at
// /etc/gitconfig, wiring the wardyn-git-helper binary into git — without it
// the binary on PATH is never called, and any BYOI run with a `github_token`
// grant fails `agent-run --selftest`.
//
// Mirrors deploy/images/base/Dockerfile's `git config --system`, with one
// deliberate difference: the secret-file path is $HOME-relative, not the
// agent images' hardcoded /home/agent, since a BYOI base has whatever user
// and home it likes. Hardcoding /home/agent here would silently disable the
// caller-auth gate on every BYOI image (the helper falls open when the file
// is absent).
const finalizeGitConfig = "[credential]\n" +
	"\thelper = /usr/local/bin/wardyn-git-helper --secret-file $HOME/.wardyn/git-helper.secret\n"

// buildFinalizeContext assembles the in-memory tar build context: the
// generated Dockerfile plus every file in toolsDir staged under tools/ with
// an executable mode so COPY preserves it.
func buildFinalizeContext(baseRef, toolsDir string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	if err := writeTarFile(tw, "Dockerfile", []byte(finalizeDockerfile(baseRef)), 0o644); err != nil {
		return nil, err
	}
	// 0644 root-owned: the sandbox user must not be able to rewrite the helper
	// path out of its own credential config.
	if err := writeTarFile(tw, "gitconfig", []byte(finalizeGitConfig), 0o644); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(toolsDir)
	if err != nil {
		return nil, fmt.Errorf("envbuild: read tools dir %q: %w", toolsDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue // flat tools dir; nested dirs aren't part of the contract
		}
		data, err := os.ReadFile(filepath.Join(toolsDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("envbuild: read tool %q: %w", e.Name(), err)
		}
		// 0755 so COPY drops the tools onto PATH already executable (no RUN chmod).
		if err := writeTarFile(tw, "tools/"+e.Name(), data, 0o755); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("envbuild: finalize tar close: %w", err)
	}
	return &buf, nil
}

// writeTarFile writes one regular file into the tar with the given mode.
func writeTarFile(tw *tar.Writer, name string, data []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     mode,
		Size:     int64(len(data)),
	}); err != nil {
		return fmt.Errorf("envbuild: tar header %q: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("envbuild: tar write %q: %w", name, err)
	}
	return nil
}
