// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistant

import (
	"regexp"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The caps on what a repository draft reads: one file, and all of them together. A larger file is skipped and
// named as skipped.
const (
	MaxBuildFileBytes  = 64 << 10
	MaxBuildFilesBytes = 256 << 10
)

// BuildFileGroup is one group of the fixed list, in the words the settings card draws.
type BuildFileGroup struct {
	Label string
	Names []string
}

// buildFileGroups is the fixed list of root files a repository draft may read. Matching is exact and
// case-sensitive, at the repository root only. It never holds a nested path, a file that names a registry's
// credentials (.npmrc, settings.xml, .yarnrc.yml), a compose, CI or build-script file, or a Gemfile, mix.exs or
// *.gradle file.
var buildFileGroups = []BuildFileGroup{
	{"Container and dev setup", []string{"Dockerfile", "Containerfile", ".devcontainer.json"}},
	{"Toolchain pins", []string{".tool-versions", ".nvmrc", ".python-version", ".ruby-version", "mise.toml", "rust-toolchain.toml", "global.json", "runtime.txt"}},
	{"Manifests", []string{"package.json", "go.mod", "Cargo.toml", "pyproject.toml", "requirements.txt", "Pipfile", "composer.json", "pom.xml", "pubspec.yaml"}},
	{"Lockfiles", []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.sum", "Cargo.lock", "poetry.lock", "pdm.lock", "uv.lock", "Pipfile.lock", "Gemfile.lock", "composer.lock", "mix.lock", "pubspec.lock"}},
}

// BuildFileGroups is the fixed list by group, in the order it is read.
func BuildFileGroups() []BuildFileGroup {
	out := make([]BuildFileGroup, len(buildFileGroups))
	for i, g := range buildFileGroups {
		out[i] = BuildFileGroup{Label: g.Label, Names: slices.Clone(g.Names)}
	}
	return out
}

// BuildFileNames is every name on the fixed list, in the order it is read.
func BuildFileNames() []string {
	var out []string
	for _, g := range buildFileGroups {
		out = append(out, g.Names...)
	}
	return out
}

// IsBuildFileName reports whether name is on the fixed list: an exact, case-sensitive match of a root file.
func IsBuildFileName(name string) bool {
	for _, g := range buildFileGroups {
		if slices.Contains(g.Names, name) {
			return true
		}
	}
	return false
}

// File is one confirmed build file of a repository draft.
type File = BuildFile

// KeepWithinCaps is the files that fit, in order, and the names of those left out: a file over
// MaxBuildFileBytes, or one that would take the total past MaxBuildFilesBytes.
func KeepWithinCaps(files []File) (kept []File, skipped []string) {
	total := 0
	for _, f := range files {
		if len(f.Text) > MaxBuildFileBytes || total+len(f.Text) > MaxBuildFilesBytes {
			skipped = append(skipped, f.Name)
			continue
		}
		total += len(f.Text)
		kept = append(kept, f)
	}
	return kept, skipped
}

// BuildFilesScanSpec is the inspection a repository read scans each file with. It is the one constructor:
// production builds its engine from it and the lockfile test scans with it. Only the catalog of well-known
// secret formats is on. Entropy is off because lockfile integrity hashes would hold back every repository, and
// no run's own secret values exist here to match.
func BuildFilesScanSpec() types.LLMInspectionSpec {
	return types.LLMInspectionSpec{
		Mode:                 "block",
		DetectSecretPatterns: true,
		BlockMinSeverity:     "low",
		OnScannerError:       "block",
		MaxScanBytes:         MaxBuildFileBytes,
	}
}

// AddBuildFiles is p with the confirmed files added as the last, untrusted section, as the first message
// carries them. Names the payload already lettered are lettered in the files too, and the caps apply.
func AddBuildFiles(p Payload, files []File) Payload {
	text := LettersFromMap(p.Letters).Apply(buildText(files))
	if strings.TrimSpace(text) == "" {
		return p
	}
	secs := append(slices.Clone(p.Sections), types.AssistantSection{Label: LabelBuild, Text: text, Untrusted: true})
	return Payload{Sections: capSections(secs), Letters: p.Letters}
}

var buildFileHeader = regexp.MustCompile(`(?m)^--- (` + strings.Join(quoteAll(BuildFileNames()), "|") + `)$`)

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = regexp.QuoteMeta(n)
	}
	return out
}

// FilesOf reads the confirmed files back from a payload's sections: the text of the build-files section split at
// its headers, which only the fixed list's names can open. A section with none yields no file.
func FilesOf(sections []types.AssistantSection) []File {
	var out []File
	for _, s := range sections {
		if s.Label != LabelBuild {
			continue
		}
		heads := buildFileHeader.FindAllStringSubmatchIndex(s.Text, -1)
		for i, h := range heads {
			start, end := h[1], len(s.Text)
			if start < end {
				start++ // the newline after the header
			}
			if i+1 < len(heads) {
				end = max(start, heads[i+1][0]-1) // the newline that joined this file to the next
			}
			out = append(out, File{Name: s.Text[h[2]:h[3]], Text: s.Text[min(start, end):end]})
		}
	}
	return out
}

// LiteralIn reports whether value appears in one of files as a whole token: the character before it and the
// one after it are not part of a host, image or package name, and a host is not followed by a further label.
// So registry.npmjs.org is in a lockfile that fetches from it, and is not in one that names
// registry.npmjs.org.evil.example. Matching is exact and case-sensitive. An empty value is in no file.
func LiteralIn(value string, files []File) bool {
	if value == "" {
		return false
	}
	for _, f := range files {
		for from := 0; ; {
			i := strings.Index(f.Text[from:], value)
			if i < 0 {
				break
			}
			at := from + i
			from = at + 1
			if wholeToken(f.Text, at, at+len(value)) {
				return true
			}
		}
	}
	return false
}

// wholeToken reports whether text[start:end] is not a piece of a longer name.
func wholeToken(text string, start, end int) bool {
	if start > 0 && (isNameByte(text[start-1]) || text[start-1] == '.' || text[start-1] == '@') {
		return false
	}
	if end < len(text) {
		if isNameByte(text[end]) {
			return false
		}
		if text[end] == '.' && end+1 < len(text) && isNameByte(text[end+1]) {
			return false
		}
	}
	return true
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}
