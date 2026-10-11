// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// The identity of an SCM entry for push rules: the provider's HOST and the
// organisation, both canonicalised. A key built from the provider kind alone
// would put github.com/acme and a GitHub Enterprise Server's acme under one
// key, and one that kept the spelling of the organisation would let "acme " or
// "Ａcme" file rules where the git route's own derivation never looks, so the
// rules would silently not apply. Authoring refuses what cannot be canonical
// (never re-keys it), and the git route derives the same pair from the remote
// with PushRuleEntryOfRemote. The TypeScript twin (pushRuleKey in the console)
// is held to the same table of cases, push-rule-key.golden.json.

// The public host of each provider, which an authored set or override that names
// no host is for.
const (
	PublicGitHubHost      = "github.com"
	PublicAzureDevOpsHost = "dev.azure.com"
)

const maxPushOrgLen = 100

var (
	pushHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	pushOrgRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// CanonicalPushOrg is the organisation's canonical spelling: Unicode NFKC, then
// ASCII lower case. It refuses what cannot be one entry's name: anything empty,
// longer than 100 bytes, holding white space or a control or format character
// (a zero-width space included), a trailing dot, or any character outside ASCII
// after the normalisation. GitHub owners and Azure DevOps organisations are
// ASCII and case-insensitive, so nothing else can name one.
func CanonicalPushOrg(org string) (string, error) {
	n := norm.NFKC.String(org)
	switch {
	case n == "":
		return "", errors.New("name the organisation")
	case len(n) > maxPushOrgLen:
		return "", fmt.Errorf("organisation is longer than %d characters", maxPushOrgLen)
	case strings.HasSuffix(n, "."):
		return "", errors.New("organisation ends with a dot")
	}
	for _, r := range n {
		if r > unicode.MaxASCII || unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("organisation holds white space, a control or zero-width character, or a character outside ASCII")
		}
	}
	n = strings.ToLower(n)
	if !pushOrgRE.MatchString(n) {
		return "", errors.New("organisation uses letters, digits, '.', '_' and '-' and starts with a letter or digit")
	}
	return n, nil
}

// CanonicalPushHost is the provider host's canonical spelling: ASCII lower case,
// no port, no trailing dot, and the ssh. alias of an SSH remote folded into the
// https host. An empty host is the provider's public host.
func CanonicalPushHost(provider, host string) (string, error) {
	if host == "" {
		switch provider {
		case string(GitProviderGitHub):
			return PublicGitHubHost, nil
		case string(GitProviderAzureDevOps):
			return PublicAzureDevOpsHost, nil
		}
		return "", fmt.Errorf("provider %q is not one of github, azure_devops", provider)
	}
	h := strings.ToLower(norm.NFKC.String(host))
	h = strings.TrimPrefix(h, "ssh.")
	if !pushHostRE.MatchString(h) || len(h) > 253 {
		return "", fmt.Errorf("host %q is not a plain DNS name (no port, white space, trailing dot or non-ASCII)", host)
	}
	return h, nil
}

// PushRuleKey is the identity of an SCM entry for push rules, "host/org", both
// canonical, or an error saying what cannot be one entry.
func PushRuleKey(provider, host, org string) (string, error) {
	if provider != string(GitProviderGitHub) && provider != string(GitProviderAzureDevOps) {
		return "", fmt.Errorf("provider %q is not one of github, azure_devops", provider)
	}
	h, err := CanonicalPushHost(provider, host)
	if err != nil {
		return "", err
	}
	o, err := CanonicalPushOrg(org)
	if err != nil {
		return "", err
	}
	return h + "/" + o, nil
}

// PushRuleEntryOfRemote is how the git route finds the entry a remote belongs
// to, from the remote's own address and never from a request body. The host is
// the remote's host, lower case, with the ssh. alias folded; the organisation is:
//
//   - GitHub and GitHub Enterprise Server: the first path segment
//     (https://host/org/repo.git, git@host:org/repo.git, ssh://git@host/org/repo).
//   - Azure DevOps: https://dev.azure.com/org/project/_git/repo (a user@ prefix
//     is ignored), https://org.visualstudio.com/project/_git/repo (host becomes
//     dev.azure.com), and the SSH forms git@ssh.dev.azure.com:v3/org/project/repo
//     and git@vs-ssh.visualstudio.com:v3/org/project/repo.
//   - Azure DevOps Server: the first path segment, the collection.
//
// A remote whose entry cannot be derived is an error, which the caller must
// treat as a refusal and never as "no rules".
func PushRuleEntryOfRemote(provider, remote string) (host, org string, err error) {
	h, path, err := splitRemote(remote)
	if err != nil {
		return "", "", err
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if provider == string(GitProviderAzureDevOps) {
		switch {
		case strings.HasSuffix(h, ".visualstudio.com") && h != "vs-ssh.visualstudio.com":
			org, h = strings.TrimSuffix(h, ".visualstudio.com"), PublicAzureDevOpsHost
		case (h == "dev.azure.com" || h == "vs-ssh.visualstudio.com") && len(segs) > 1 && segs[0] == "v3":
			org, h = segs[1], PublicAzureDevOpsHost
		case h == "vs-ssh.visualstudio.com":
			return "", "", fmt.Errorf("remote %q has no v3/{organisation} path", remote)
		}
	}
	if org == "" {
		org = segs[0]
	}
	if _, err := PushRuleKey(provider, h, org); err != nil {
		return "", "", fmt.Errorf("remote %q: %w", remote, err)
	}
	return h, org, nil
}

// splitRemote returns the lower-case host (ssh. alias folded) and path of a
// remote in URL or scp-like form.
func splitRemote(remote string) (host, path string, err error) {
	var raw string
	if strings.Contains(remote, "://") {
		u, perr := url.Parse(remote)
		if perr != nil {
			return "", "", fmt.Errorf("remote %q is not a URL", remote)
		}
		host, path = u.Hostname(), u.EscapedPath()
		raw = host
	} else {
		at := strings.LastIndex(remote, "@")
		hostPath := remote[at+1:]
		var ok bool
		if host, path, ok = strings.Cut(hostPath, ":"); !ok {
			return "", "", fmt.Errorf("remote %q is neither a URL nor user@host:path", remote)
		}
		raw = host
	}
	host = strings.TrimSuffix(strings.ToLower(raw), ".")
	host = strings.TrimPrefix(host, "ssh.")
	if host == "" || strings.Trim(path, "/") == "" {
		return "", "", fmt.Errorf("remote %q has no host or path", remote)
	}
	return host, path, nil
}
