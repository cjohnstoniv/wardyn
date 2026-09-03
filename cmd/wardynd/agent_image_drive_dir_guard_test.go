// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// agentHome is the sandbox user's home: the directory the OWNERSHIP half of this
// guard is about, and the parent of driveDir.
const agentHome = "/home/agent"

// driveDir is the reserved in-container user-drive mount point
// (runner.DriveTarget). Spelled literally rather than imported: this guard
// reads Dockerfiles, and the whole point is to catch an image whose text does
// not carry the path.
const driveDir = agentHome + "/drive"

// wardynSiblingRef matches a base that is another image in this directory —
// `wardyn/agent-<name>:<tag>`. Those images inherit the drive directory from
// their parent and must not re-create it.
var wardynSiblingRef = regexp.MustCompile(`^wardyn/agent-([a-z0-9-]+):`)

// dockerStage is one build stage: the image (or earlier stage) its FROM names,
// the `AS <alias>` it answers to, and the instructions between this FROM and
// the next.
type dockerStage struct {
	base   string
	alias  string
	instrs []string
}

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
//
// IT IS THE FINAL STAGE THAT SHIPS, so the walk starts there and follows only
// that stage's own ancestry. A multi-stage Dockerfile's builder stages are
// thrown away: a `mkdir /home/agent/drive` in one of them creates a directory
// in a layer no container ever runs, and reading every stage's instructions as
// one bag (which this guard used to do) let such a line satisfy a runtime stage
// that has none. The same applies to the FROM chain — an earlier
// `FROM wardyn/agent-base:local AS tools` says nothing about a final stage
// built on debian, so the parent hop is taken from the FINAL stage's base only,
// resolving an alias to the stage it names before treating a ref as a sibling
// image.
func TestAgentImagesPreCreateDriveDir(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "deploy", "images")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read deploy/images: %v", err)
	}

	// image name -> its build stages, in file order (the last is what ships).
	images := map[string][]dockerStage{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name(), "Dockerfile"))
		if rerr != nil {
			continue // common/ and any other non-image directory
		}
		stages := dockerfileStages(string(raw))
		if len(stages) == 0 {
			t.Fatalf("%s/Dockerfile parsed to zero stages — the FROM scan regressed", e.Name())
		}
		images[e.Name()] = stages
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
	derived := 0
	total := 0
	for _, stages := range images {
		total += len(stages)
		if wardynSiblingRef.MatchString(stages[len(stages)-1].base) {
			derived++
		}
	}
	if derived == 0 {
		t.Fatal("no image's FINAL stage is built `FROM wardyn/agent-…` — the FROM-chain parse regressed, and a derived image missing the drive dir would no longer be traced to the ancestor that creates it")
	}
	if total <= len(images) {
		t.Fatalf("every image parsed to a single stage (%d stages over %d images) — the stage split regressed, and a builder-stage mkdir would satisfy a runtime stage again", total, len(images))
	}

	for _, name := range sortedKeys(images) {
		t.Run(name, func(t *testing.T) {
			// Walk from the FINAL stage up its own ancestry — an earlier stage
			// only when this one's FROM names it, a sibling image only when this
			// one's FROM is that image. Bounded by the total number of stages, so
			// a cycle cannot hang the test.
			cur, stages, idx := name, images[name], len(images[name])-1
			for hops := 0; hops <= total; hops++ {
				st := stages[idx]
				if mk := driveMkdirInstruction(st.instrs); mk != "" {
					// BOTH FORMS. The substring pins the SPELLING the images
					// actually use (flag, owner, path, in that order);
					// chownsAgentHome pins that the path is /home/agent ITSELF
					// and not a child of it — `chown -R agent:agent
					// /home/agent/work` satisfies the substring and leaves the
					// drive directory root-owned.
					if !strings.Contains(mk, "chown -R agent:agent "+agentHome) || !chownsAgentHome(mk) {
						t.Errorf("%s: %s is created but the SAME instruction does not chown %s to agent:\n\t%s\n"+
							"A root-owned drive root is EACCES for uid 1000 the first time a managed drive is mounted over it.", cur, driveDir, agentHome, mk)
					}
					return
				}
				// An alias of an EARLIER stage in this same file. Only earlier —
				// a FROM can never name a stage declared below it.
				if i := stageByAlias(stages, st.base); i >= 0 && i < idx {
					idx = i
					continue
				}
				if m := wardynSiblingRef.FindStringSubmatch(st.base); m != nil {
					next, ok := images[m[1]]
					if !ok {
						t.Fatalf("%s: FROM chain names %s, which has no Dockerfile under deploy/images", name, st.base)
					}
					cur, stages, idx = m[1], next, len(next)-1
					continue
				}
				t.Errorf("%s: the stage that SHIPS (built FROM %s) does not create %s, and neither does any stage or image it is built on.\n"+
					"Add it to that stage's `mkdir -p /home/agent/work` line (owned by agent, same RUN as the chown): a managed\n"+
					"docker_volume drive mounted here would otherwise get a ROOT-owned volume root and be unwritable by uid 1000.\n"+
					"A mkdir in a BUILDER stage does not count — that layer is thrown away.", name, st.base, driveDir)
				return
			}
			t.Errorf("%s: FROM chain did not terminate — a cycle among deploy/images Dockerfiles?", name)
		})
	}
}

// dockerfileStages splits a Dockerfile into its stages: each FROM starts one,
// and the instructions after it belong to it. ARG lines before the first FROM
// are global and belong to no stage, so they are dropped.
func dockerfileStages(src string) []dockerStage {
	var out []dockerStage
	for _, in := range dockerfileInstructions(src) {
		fields := strings.Fields(in)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
			if len(out) > 0 {
				last := &out[len(out)-1]
				last.instrs = append(last.instrs, in)
			}
			continue
		}
		st := dockerStage{}
		rest := fields[1:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "--") { // --platform=…
			rest = rest[1:]
		}
		if len(rest) == 0 {
			continue
		}
		st.base = strings.ToLower(rest[0])
		if len(rest) >= 3 && strings.EqualFold(rest[1], "AS") {
			st.alias = strings.ToLower(rest[2])
		}
		out = append(out, st)
	}
	return out
}

// stageByAlias returns the index of the stage answering to ref, or -1.
func stageByAlias(stages []dockerStage, ref string) int {
	for i, st := range stages {
		if st.alias != "" && st.alias == ref {
			return i
		}
	}
	return -1
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
		if driveMkdirSegment(in) != "" {
			return in
		}
	}
	return ""
}

// shellWords splits one `&&`-joined segment into its words, dropping the leading
// Dockerfile verb (`RUN`) that only the first segment carries.
func shellWords(seg string) []string {
	fields := strings.Fields(seg)
	if len(fields) > 0 && strings.EqualFold(fields[0], "RUN") {
		fields = fields[1:]
	}
	return fields
}

// driveMkdirSegment returns the `&&`-joined segment of one instruction that
// really creates the drive directory, or "".
//
// THE SEGMENT, AND THE PATH AS A WHOLE WORD. `strings.Contains(in, "mkdir") &&
// strings.Contains(in, driveDir)` over the whole instruction was true for two
// shapes that create nothing at the reserved path:
//
//   - a SAME-PREFIX SIBLING. `mkdir -p /home/agent/work /home/agent/drive-cache`
//     contains the substring "/home/agent/drive" and leaves the reserved path
//     uncreated, so a managed volume still copies up onto a ROOT-owned directory
//     and a writable drive is EACCES for uid 1000 — the exact failure this file
//     exists to make loud.
//   - a MENTION rather than a creation. `RUN mkdir -p /home/agent/work && echo
//     "would mkdir /home/agent/drive"` satisfies both substrings across two
//     different segments, and creates only the work directory.
//
// So: the path must be a whole ARGUMENT (strings.Fields, never a substring) of a
// segment whose COMMAND is mkdir.
func driveMkdirSegment(instr string) string {
	for _, seg := range strings.Split(instr, "&&") {
		words := shellWords(seg)
		if len(words) == 0 || filepath.Base(words[0]) != "mkdir" {
			continue
		}
		if slices.Contains(words[1:], driveDir) {
			return strings.TrimSpace(seg)
		}
	}
	return ""
}

// chownsAgentHome reports whether one instruction hands /home/agent ITSELF to
// agent, recursively — the half that matters, because a drive directory the
// image creates but leaves owned by root is exactly as unwritable as one it
// never created.
//
// A COMPLETE WORD, never a substring: `strings.Contains(mk, "chown -R
// agent:agent /home/agent")` was satisfied by `chown -R agent:agent
// /home/agent/work`, which is the drift this file was written for in the first
// place — the work directory chowned, the drive directory left to root.
func chownsAgentHome(instr string) bool {
	for _, seg := range strings.Split(instr, "&&") {
		words := shellWords(seg)
		if len(words) == 0 || filepath.Base(words[0]) != "chown" {
			continue
		}
		if slices.Contains(words, "-R") && slices.Contains(words, "agent:agent") && slices.Contains(words, agentHome) {
			return true
		}
	}
	return false
}

// sortedKeys returns a map's keys in a stable order, so subtest names and
// failure messages do not shuffle between runs.
func sortedKeys(m map[string][]dockerStage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestDockerfileStages_ABuilderStageDoesNotCountForTheRuntimeStage is the
// counterfactual for the walk above, on a Dockerfile no image in the tree has —
// and the exact shape that used to pass wrongly. Reading every instruction as
// one bag found the builder's mkdir; taking the LAST `FROM wardyn/agent-…`
// found the builder's parent. Both answers describe a layer that is thrown
// away, while the image that actually ships has a root-owned /home/agent/drive.
func TestDockerfileStages_ABuilderStageDoesNotCountForTheRuntimeStage(t *testing.T) {
	const src = `
FROM wardyn/agent-base:local AS tools
RUN mkdir -p /home/agent/work /home/agent/drive \
 && chown -R agent:agent /home/agent
FROM debian:bookworm-slim
COPY --from=tools /usr/local/bin/thing /usr/local/bin/thing
RUN mkdir -p /home/agent/work && chown -R agent:agent /home/agent
`
	stages := dockerfileStages(src)
	if len(stages) != 2 {
		t.Fatalf("parsed %d stages, want 2: %+v", len(stages), stages)
	}
	if stages[0].alias != "tools" || stages[0].base != "wardyn/agent-base:local" {
		t.Errorf("builder stage = %+v, want base wardyn/agent-base:local AS tools", stages[0])
	}
	final := stages[len(stages)-1]
	if final.base != "debian:bookworm-slim" {
		t.Errorf("final stage base = %q, want the EXTERNAL base — the last `FROM wardyn/agent-…` is not the final stage", final.base)
	}
	if mk := driveMkdirInstruction(final.instrs); mk != "" {
		t.Errorf("the final stage was credited with %q, which belongs to the builder stage", mk)
	}
	if mk := driveMkdirInstruction(stages[0].instrs); mk == "" {
		t.Error("the builder stage's own mkdir went missing — the split dropped instructions")
	}
	if i := stageByAlias(stages, final.base); i != -1 {
		t.Errorf("the external base resolved to in-file stage %d — an alias lookup must not match an image reference", i)
	}
	// And a --platform flag on the FROM does not become the base.
	if got := dockerfileStages("FROM --platform=$BUILDPLATFORM golang:1.27 AS builder\nRUN true\n"); len(got) != 1 || got[0].base != "golang:1.27" || got[0].alias != "builder" {
		t.Errorf("--platform FROM parsed as %+v, want base golang:1.27 alias builder", got)
	}
}

// TestDriveDirGuard_RefusesLookalikes is the counterfactual for the two
// predicates the walk above is built on — the shapes that used to satisfy a
// substring test while shipping an image whose /home/agent/drive is root-owned
// or absent. Every "want false" row here is an image that would have graded
// green.
func TestDriveDirGuard_RefusesLookalikes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		instr string
		want  bool
	}{
		{"the real thing", "RUN mkdir -p /home/agent/work /home/agent/drive && chown -R agent:agent /home/agent", true},
		{"the drive alone", "RUN mkdir -p /home/agent/drive", true},
		// A directory whose name merely STARTS with the reserved path. The
		// reserved path is never created, so the volume's copy-up still lands on
		// a root-owned directory.
		{"a same-prefix sibling", "RUN mkdir -p /home/agent/work /home/agent/drive-cache && chown -R agent:agent /home/agent", false},
		{"a same-prefix child", "RUN mkdir -p /home/agent/drive/inbox", false},
		// "mkdir" in one segment, the path in another: both substrings present,
		// nothing created at the reserved path.
		{"a mention, not a creation", `RUN mkdir -p /home/agent/work && echo "would mkdir /home/agent/drive"`, false},
		{"an unquoted mention", "RUN mkdir -p /home/agent/work && echo would mkdir /home/agent/drive", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := driveMkdirSegment(tc.instr) != ""; got != tc.want {
				t.Errorf("driveMkdirSegment(%q) creates %s = %v, want %v", tc.instr, driveDir, got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		instr string
		want  bool
	}{
		{"the real thing", "RUN mkdir -p /home/agent/drive && chown -R agent:agent /home/agent", true},
		// THE ORIGINAL HOLE: a chown of a CHILD of the home satisfies the
		// substring `chown -R agent:agent /home/agent` and leaves the drive
		// directory owned by root.
		{"a child of the home", "RUN mkdir -p /home/agent/drive && chown -R agent:agent /home/agent/work", false},
		{"not recursive", "RUN mkdir -p /home/agent/drive && chown agent:agent /home/agent", false},
		{"the wrong owner", "RUN mkdir -p /home/agent/drive && chown -R root:root /home/agent", false},
		{"a mention, not a chown", `RUN mkdir -p /home/agent/drive && echo "chown -R agent:agent /home/agent"`, false},
	} {
		t.Run("chown/"+tc.name, func(t *testing.T) {
			if got := chownsAgentHome(tc.instr); got != tc.want {
				t.Errorf("chownsAgentHome(%q) = %v, want %v", tc.instr, got, tc.want)
			}
		})
	}
}
