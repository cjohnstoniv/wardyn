// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package groundtruth

import "strings"

// SensitivePathFilter decides which file-write paths are worth recording. The
// kernel sensor sees EVERY write; recording all of them would drown the audit
// stream and add no signal. We record only writes to credential-shaped and
// security-relevant paths — the locations an exfiltrating or self-persisting
// agent actually touches.
//
// This is an ALLOWLIST in the sense of "paths we allow into the audit stream",
// not a security control: it is a noise filter on a DETECTION stream. A miss
// here loses telemetry, never enforcement.
//
// HONEST RESIDUAL: matching is path-shape based. An agent that writes a
// credential to an unconventional path evades the filter (but not the kernel,
// which still saw it — the filter is what we chose to record). The
// TracingPolicy on the sensor side narrows what the kernel even reports; this
// is the second, in-process narrowing.

// sensitivePrefixes match by absolute-path prefix.
var sensitivePrefixes = []string{
	"/etc/",                 // system config (passwd, shadow, sudoers, ...)
	"/root/.ssh/",           // root ssh keys/config
	"/root/.aws/",           // root cloud creds
	"/root/.config/gcloud/", // root gcloud creds
	"/root/.kube/",          // kubeconfig
	"/root/.docker/",        // docker config (registry creds)
	"/var/run/secrets/",     // k8s mounted service-account tokens
	"/run/secrets/",         // docker/compose mounted secrets
}

// sensitiveSubstrings match anywhere in the path. They cover home directories
// under any /home/<user> or arbitrary workspace root without enumerating users.
var sensitiveSubstrings = []string{
	"/.ssh/",               // ~/.ssh/id_*, known_hosts, config
	"/.aws/",               // ~/.aws/credentials, config
	"/.config/gcloud/",     // ~/.config/gcloud/*
	"/.kube/",              // ~/.kube/config
	"/.docker/config.json", // docker registry auth
	"/.git/config",         // git remotes/credentials helper
	"/.gitconfig",          // user-level git config (credential.helper)
	"/.git-credentials",    // plaintext git credentials store
	"/.netrc",              // machine creds
	"/.npmrc",              // npm auth tokens
	"/.pypirc",             // pypi upload tokens
	"/.gnupg/",             // gpg keys
}

// sensitiveSuffixes match credential-shaped basenames anywhere on disk.
var sensitiveSuffixes = []string{
	".pem",
	".key",
	"id_rsa",
	"id_ed25519",
	"id_ecdsa",
	"credentials",
	".token",
	"token.json",
	"service-account.json",
}

// IsSensitivePath reports whether a written file path is in the allowlist of
// paths we record on the file_write subtype. Empty paths are not sensitive.
func IsSensitivePath(path string) bool {
	p := strings.TrimSpace(path)
	if p == "" {
		return false
	}
	for _, pre := range sensitivePrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	for _, sub := range sensitiveSubstrings {
		if strings.Contains(p, sub) {
			return true
		}
	}
	for _, suf := range sensitiveSuffixes {
		if strings.HasSuffix(p, suf) {
			return true
		}
	}
	return false
}
