// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Values of a git_pat scope's access and forge axes.
const (
	PATAccessRead  = "read"
	PATAccessWrite = "write"

	PATForgeGeneric         = "generic"
	PATForgeGitLab          = "gitlab"
	PATForgeBitbucketServer = "bitbucket_server"
	PATForgeGitea           = "gitea"
)

// GitPATScope is the one decoded shape of a git_pat grant scope. api, broker
// and composer all read it, so the contract is defined once.
//
// The four narrowing axes have explicit omission defaults (Normalize):
//
//	repos   absent = every repository the PAT reaches; an EMPTY list = none
//	access  write
//	api     false
//	forge   generic
//
// Repos is a pointer to a slice so "absent" (nil) and "empty" (non-nil, no
// entries) stay distinct. Reading an empty list as unset would turn an empty
// intersection into an unnarrowed grant.
//
// Wardyn narrows the run, not the PAT: the credential itself keeps whatever
// scope the forge issued it with.
type GitPATScope struct {
	Host       string    `json:"host"`
	SecretName string    `json:"secret_name"`
	Username   string    `json:"username,omitempty"`
	Repos      *[]string `json:"repos,omitempty"`
	Access     string    `json:"access,omitempty"`
	API        bool      `json:"api,omitempty"`
	Forge      string    `json:"forge,omitempty"`
}

// PATRepoSet returns a Repos value holding exactly the given entries (an empty
// call is the explicit empty list, "none").
func PATRepoSet(entries ...string) *[]string {
	if entries == nil {
		entries = []string{}
	}
	return &entries
}

// Normalize applies the omission defaults and fails closed on a value outside
// an enum: an unknown access reads as read, an unknown forge as generic (the
// strictest path identity). Strict decoding refuses both before they can be
// stored; this only matters for a row that predates the check. Repos is
// returned as is, because absent and empty mean different things.
func (s GitPATScope) Normalize() GitPATScope {
	switch strings.ToLower(strings.TrimSpace(s.Access)) {
	case "", PATAccessWrite:
		s.Access = PATAccessWrite
	default:
		s.Access = PATAccessRead
	}
	switch f := strings.ToLower(strings.TrimSpace(s.Forge)); f {
	case PATForgeGitLab, PATForgeBitbucketServer, PATForgeGitea:
		s.Forge = f
	default:
		s.Forge = PATForgeGeneric
	}
	return s
}

// Narrowed reports whether the scope narrows the run below the PAT: it sets
// repos (an empty list included), sets access read, or sets api true. Call it on
// a normalized scope.
func (s GitPATScope) Narrowed() bool {
	return s.Repos != nil || s.Access == PATAccessRead || s.API
}

// SetsAnyAxis is Narrowed, or a forge other than generic: the test for a scope
// a lane that ignores the four axes must refuse rather than carry silently.
func (s GitPATScope) SetsAnyAxis() bool {
	return s.Narrowed() || s.Forge != PATForgeGeneric
}

// DecodeGitPATScope is the lenient read: unknown keys are ignored, so a stored
// row with a stray key still loads and launches. host and secret_name are
// required (fail closed). The result is normalized.
func DecodeGitPATScope(raw json.RawMessage) (GitPATScope, error) {
	var sc GitPATScope
	if err := json.Unmarshal(raw, &sc); err != nil {
		return GitPATScope{}, err
	}
	if sc.Host == "" || sc.SecretName == "" {
		return GitPATScope{}, errors.New("git_pat scope requires host and secret_name")
	}
	return sc.Normalize(), nil
}

// DecodeGitPATScopeStrict is the write-time decode, for request bodies and the
// boot policy file. Beyond DecodeGitPATScope it refuses an unknown key (a typo
// such as "repo" would otherwise read as an omission, and an omission means
// unnarrowed), an access or forge outside its enum, a malformed repos entry,
// and api: true on the generic forge, which has no API table. Whether a
// Bitbucket Server API grant is allowed is a deployment flag, which the api
// package checks at write.
func DecodeGitPATScopeStrict(raw json.RawMessage) (GitPATScope, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var sc GitPATScope
	if err := dec.Decode(&sc); err != nil {
		return GitPATScope{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return GitPATScope{}, errors.New("unexpected data after the scope object")
	}
	if sc.Host == "" || sc.SecretName == "" {
		return GitPATScope{}, errors.New("git_pat scope requires host and secret_name")
	}
	switch sc.Access {
	case "", PATAccessRead, PATAccessWrite:
	default:
		return GitPATScope{}, fmt.Errorf("git_pat scope access %q is not one of read, write", sc.Access)
	}
	switch sc.Forge {
	case "", PATForgeGeneric, PATForgeGitLab, PATForgeBitbucketServer, PATForgeGitea:
	default:
		return GitPATScope{}, fmt.Errorf("git_pat scope forge %q is not one of generic, gitlab, bitbucket_server, gitea", sc.Forge)
	}
	sc = sc.Normalize()
	if sc.Repos != nil {
		for _, e := range *sc.Repos {
			if _, ok := PATRepoKey(sc.Forge, e); !ok {
				return GitPATScope{}, fmt.Errorf("git_pat scope repos entry %q is malformed (non-empty, no \"..\", \"?\", \"#\", backslash or control character, no empty segment, at most one trailing \"/*\")", e)
			}
		}
	}
	if sc.API && sc.Forge == PATForgeGeneric {
		return GitPATScope{}, errors.New("git_pat scope api: true is not available on the generic forge (set forge to gitlab or gitea)")
	}
	return sc, nil
}

// PATRepoKey is the one repository-identity function for a git_pat scope. It
// returns the key a repos entry (or an extracted request path) compares by, and
// ok=false for a malformed one: empty, an empty segment, ".." anywhere, a "."
// segment, "?", "#", a backslash, a control character, an empty or repeated
// trailing "/*", or an unknown forge. An empty forge means generic.
//
// generic returns the path exactly: no case-folding, no ".git" stripping,
// because git-http-backend serves different repositories for Team/App.git and
// team/app.git, and for team/foo and team/foo.git. A forge may canonicalise
// only where the proxy then forwards the canonical form, so that the compared
// string and the forwarded string are one value. No forge does yet, so every
// forge keys by the exact path.
func PATRepoKey(forge, path string) (key string, ok bool) {
	switch forge {
	case "", PATForgeGeneric, PATForgeGitLab, PATForgeBitbucketServer, PATForgeGitea:
	default:
		return "", false
	}
	if path == "" || strings.Contains(path, "..") {
		return "", false
	}
	for _, c := range path {
		if c < 0x20 || c == 0x7f || c == '#' || c == '?' || c == '\\' {
			return "", false
		}
	}
	base := path
	if b, wild := strings.CutSuffix(path, "/*"); wild {
		base = b
	}
	if base == "" || strings.HasSuffix(base, "/*") {
		return "", false
	}
	for _, seg := range strings.Split(base, "/") {
		if seg == "" || seg == "." {
			return "", false
		}
	}
	return path, true
}

// PATRepoCovers reports whether the repos entry key entry covers key, both
// from PATRepoKey. An entry covers itself, and one trailing "/*" covers exactly
// one further path segment: "group/*" covers "group/app" and not "group/sub/app".
// A wildcard entry is covered only by the same wildcard.
func PATRepoCovers(entry, key string) bool {
	if entry == key {
		return true
	}
	base, wild := strings.CutSuffix(entry, "/*")
	if !wild || strings.HasSuffix(key, "/*") {
		return false
	}
	rest, ok := strings.CutPrefix(key, base+"/")
	return ok && rest != "" && !strings.Contains(rest, "/")
}
