// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// driveDir is the reserved in-container user-drive mount point
// (runner.DriveTarget). Spelled literally rather than imported: this guard
// reads Dockerfiles, and the whole point is to catch an image whose text does
// not carry the path.
const driveDir = "/home/agent/drive"

// wardynBaseFROM matches the final stage of an image that is built ON another
// image in this directory — `FROM wardyn/agent-<name>:<tag>`. Those images
// inherit the drive directory from their parent and must not re-create it.
var wardynBaseFROM = regexp.MustCompile(`(?m)^FROM\s+(?:--\S+\s+)*wardyn/agent-([a-z0-9-]+):`)

// TestAgentImagesPreCreateDriveDir asserts that EVERY image under
// deploy/images ends up with /home/agent/drive present and owned by agent —
// directly, or by inheriting from a sibling image that does.
//
// WHY THIS IS A GUARD AND NOT A COMMENT. A managed (`docker_volume`) drive is
// mounted over this path, and Docker's copy-up gives the fresh volume the
// uid/gid of the image directory it lands on. An image that never creates the
// directory gets one conjured by the daemon at mount time — owned by ROOT — so
// a drive an admin allocated WRITABLE is EACCES for uid 1000 on its very first
// run. Nothing else catches it: the image builds, the container starts, the
// mount succeeds, and the failure surfaces as the agent being unable to write
// to its own drive. wardynd cannot repair it either — fixing ownership at run
// time would mean chowning volume state, which the control plane must never do
// (deploy/images/base/Dockerfile states the full argument).
//
// This originally held for `base` and `oracle` only; `claude-code`, `codex-cli`
// and `aws-sso` each created `/home/agent/work` alone, which is the drift this
// test exists to make loud. deploy/images/README.md's image contract §5 and
// "Adding a new agent image" step 5 are the prose half.
func TestAgentImagesPreCreateDriveDir(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "deploy", "images")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read deploy/images: %v", err)
	}

	// image name -> its Dockerfile, joined into logical instructions.
	images := map[string][]string{}
	// image name -> the sibling image its final stage is built FROM ("" = none).
	parent := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name(), "Dockerfile"))
		if rerr != nil {
			continue // common/ and any other non-image directory
		}
		images[e.Name()] = dockerfileInstructions(string(raw))
		if m := wardynBaseFROM.FindAllStringSubmatch(string(raw), -1); len(m) > 0 {
			// The LAST such FROM is the final (runtime) stage's base.
			parent[e.Name()] = m[len(m)-1][1]
		}
	}

	// Vacuity guards: a parse that silently found nothing would pass forever.
	if len(images) < 5 {
		t.Fatalf("found only %d agent images under deploy/images (%v) — the Dockerfile scan regressed and this guard would pass vacuously", len(images), sortedKeys(images))
	}
	for _, must := range []string{"base", "oracle", "claude-code", "codex-cli", "aws-sso"} {
		if _, ok := images[must]; !ok {
			t.Fatalf("image %q not found under deploy/images (found %v) — if it was renamed or removed, re-derive this guard", must, sortedKeys(images))
		}
	}
	if len(parent) == 0 {
		t.Fatal("no image resolved a `FROM wardyn/agent-…` parent — the FROM-chain parse regressed, and a derived image missing the drive dir would no longer be traced to the ancestor that creates it")
	}

	for _, name := range sortedKeys(images) {
		t.Run(name, func(t *testing.T) {
			// Walk the FROM chain until an ancestor creates the directory.
			// Bounded by the number of images, so a cycle cannot hang the test.
			for cur, hops := name, 0; hops <= len(images); hops++ {
				instrs, ok := images[cur]
				if !ok {
					t.Fatalf("%s: FROM chain names wardyn/agent-%s, which has no Dockerfile under deploy/images", name, cur)
				}
				if mk := driveMkdirInstruction(instrs); mk != "" {
					if !strings.Contains(mk, "chown -R agent:agent /home/agent") {
						t.Errorf("%s: %s is created but the SAME instruction does not chown it to agent:\n\t%s\n"+
							"A root-owned drive root is EACCES for uid 1000 the first time a managed drive is mounted over it.", cur, driveDir, mk)
					}
					return
				}
				next, ok := parent[cur]
				if !ok {
					t.Errorf("%s: no stage creates %s and it is not built FROM a sibling image that does.\n"+
						"Add it to the image's `mkdir -p /home/agent/work` line (owned by agent, same RUN as the chown): a managed\n"+
						"docker_volume drive mounted here would otherwise get a ROOT-owned volume root and be unwritable by uid 1000.", name, driveDir)
					return
				}
				cur = next
			}
			t.Errorf("%s: FROM chain did not terminate — a cycle among deploy/images Dockerfiles?", name)
		})
	}
}

// dockerfileInstructions joins backslash-continued lines into one string per
// logical instruction, so a `RUN … && mkdir -p … && chown …` block is a single
// entry. The mkdir and the chown being in the SAME instruction is what this
// guard actually checks — the ownership is the half that matters.
func dockerfileInstructions(src string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if cur.Len() == 0 && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue // comments and blanks are not instructions
		}
		cur.WriteString(" ")
		cur.WriteString(strings.TrimSuffix(trimmed, "\\"))
		if !strings.HasSuffix(trimmed, "\\") {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		out = append(out, strings.TrimSpace(cur.String()))
	}
	return out
}

// driveMkdirInstruction returns the instruction that mkdirs the drive
// directory, or "" when no instruction does.
func driveMkdirInstruction(instrs []string) string {
	for _, in := range instrs {
		if strings.Contains(in, "mkdir") && strings.Contains(in, driveDir) {
			return in
		}
	}
	return ""
}

// sortedKeys returns a map's keys in a stable order, so subtest names and
// failure messages do not shuffle between runs.
func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
