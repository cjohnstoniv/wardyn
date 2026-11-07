// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"strconv"
	"strings"
	"unicode"
)

// A signer's images entry uses the image-grant grammar: "<registry host>/<path>/*" covers every repository
// below that path, and any other value covers only the reference typed exactly so. Nothing is normalised, so
// a host-less "team/*" can never name Docker Hub by accident and "REGISTRY.example.com/x" is not
// "registry.example.com/x". This is a copy of the grant matcher in internal/api/base_images.go
// (imagePrefixGrant, validImagePrefix, imageRepoAsTyped, imagePrefixCovers), which this light package cannot
// import; the two must stay the same.

// imagesEntryValid reports whether e is a well-formed images entry: a prefix, or a plausible exact reference.
func imagesEntryValid(e string) bool {
	if e != strings.TrimSpace(e) || e == "" {
		return false
	}
	if _, ok := imagePrefix(e); ok {
		return true
	}
	_, ok := repoAsTyped(e)
	return ok
}

// imagesEntryCovers reports whether entry e covers the image reference ref.
func imagesEntryCovers(e, ref string) bool {
	if prefix, ok := imagePrefix(e); ok {
		repo, ok := repoAsTyped(ref)
		return ok && len(repo) > len(prefix)+1 && strings.HasPrefix(repo, prefix+"/")
	}
	if strings.Contains(e, "*") {
		return false
	}
	return e == strings.TrimSpace(ref)
}

// imagePrefix returns the part of a prefix entry before "/*".
func imagePrefix(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasSuffix(v, "/*") {
		return "", false
	}
	prefix := strings.TrimSuffix(v, "/*")
	return prefix, validPrefix(prefix)
}

// validPrefix: a path-safe "host/seg/..." whose first segment is a registry host (a "." or ":" in it, or
// "localhost"), every segment ASCII [a-z0-9._-], the host with at most one ":port" (1..65535).
func validPrefix(prefix string) bool {
	if !pathSafe(prefix) {
		return false
	}
	segs := strings.Split(prefix, "/")
	host := segs[0]
	if !(strings.ContainsAny(host, ".:") || host == "localhost") || strings.Count(host, ":") > 1 {
		return false
	}
	name, port, hasPort := strings.Cut(host, ":")
	if hasPort {
		n, err := strconv.Atoi(port)
		if port == "" || strings.Trim(port, "0123456789") != "" || err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if strings.Trim(name, ".") == "" || !nameChars(name) {
		return false
	}
	for _, seg := range segs[1:] {
		if !nameChars(seg) {
			return false
		}
	}
	return true
}

// nameChars: a non-empty run of ASCII [a-z0-9._-].
func nameChars(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// pathSafe: no backslash and no empty, "." or ".." segment.
func pathSafe(ref string) bool {
	if strings.Contains(ref, `\`) {
		return false
	}
	for _, seg := range strings.Split(strings.TrimSpace(ref), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// repoAsTyped is ref with its @digest and :tag removed, as typed. ok is false for a reference that cannot be
// an image: whitespace, a "*", an unsafe path, a digest holding a "/", or a ":" outside the host and the last
// segment.
func repoAsTyped(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if strings.ContainsFunc(ref, unicode.IsSpace) || strings.Contains(ref, "*") || !pathSafe(ref) {
		return "", false
	}
	if at := strings.Index(ref, "@"); at >= 0 {
		if strings.Contains(ref[at:], "/") {
			return "", false
		}
		ref = ref[:at]
	}
	segs := strings.Split(ref, "/")
	last := len(segs) - 1
	for i := 1; i < last; i++ {
		if strings.Contains(segs[i], ":") {
			return "", false
		}
	}
	if last > 0 {
		segs[last], _, _ = strings.Cut(segs[last], ":")
	}
	return strings.Join(segs, "/"), true
}
