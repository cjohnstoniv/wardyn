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
// no port, no trailing dot. The SSH aliases of the two public hosts are folded
// into the https host (ssh.github.com, ssh.dev.azure.com), and an Azure DevOps
// remote's legacy hosts too (*.visualstudio.com, vs-ssh.visualstudio.com become
// dev.azure.com), because one forge answers to each. Any other ssh. host is its
// own host. A host outside ASCII is refused before anything is lower-cased or
// normalised, because Go and JavaScript fold such letters differently. An empty
// host is the provider's public host.
func CanonicalPushHost(provider, host string) (string, error) {
	h, _, err := canonicalPushHostOrg(provider, host)
	return h, err
}

// canonicalPushHostOrg is CanonicalPushHost plus, for an Azure DevOps
// {org}.visualstudio.com host, the organisation its subdomain names.
func canonicalPushHostOrg(provider, host string) (h, hostOrg string, err error) {
	if host == "" {
		switch provider {
		case string(GitProviderGitHub):
			return PublicGitHubHost, "", nil
		case string(GitProviderAzureDevOps):
			return PublicAzureDevOpsHost, "", nil
		}
		return "", "", fmt.Errorf("provider %q is not one of github, azure_devops", provider)
	}
	for _, r := range host {
		if r > unicode.MaxASCII {
			return "", "", fmt.Errorf("host %q is not a plain DNS name (no port, white space, trailing dot or non-ASCII)", host)
		}
	}
	h = strings.ToLower(host)
	switch h {
	case "ssh.github.com":
		h = PublicGitHubHost
	case "ssh.dev.azure.com":
		h = PublicAzureDevOpsHost
	}
	if !pushHostRE.MatchString(h) || len(h) > 253 {
		return "", "", fmt.Errorf("host %q is not a plain DNS name (no port, white space, trailing dot or non-ASCII)", host)
	}
	if provider == string(GitProviderAzureDevOps) {
		if h == azureLegacySSHHost {
			h = PublicAzureDevOpsHost
		} else if sub := strings.TrimSuffix(h, azureLegacySuffix); sub != h {
			h, hostOrg = PublicAzureDevOpsHost, sub
		}
	}
	return h, hostOrg, nil
}

const (
	azureLegacySuffix  = ".visualstudio.com"
	azureLegacySSHHost = "vs-ssh.visualstudio.com"
)

// canonicalPushPair is the canonical host and organisation, or an error saying
// what cannot be one entry. An {org}.visualstudio.com host names its own
// organisation, so a different one is a contradiction and refused.
func canonicalPushPair(provider, host, org string) (h, o string, err error) {
	if provider != string(GitProviderGitHub) && provider != string(GitProviderAzureDevOps) {
		return "", "", fmt.Errorf("provider %q is not one of github, azure_devops", provider)
	}
	h, hostOrg, err := canonicalPushHostOrg(provider, host)
	if err != nil {
		return "", "", err
	}
	if o, err = CanonicalPushOrg(org); err != nil {
		return "", "", err
	}
	if hostOrg != "" && hostOrg != o {
		return "", "", fmt.Errorf("host %q belongs to organisation %q, not %q", host, hostOrg, org)
	}
	return h, o, nil
}

// PushRuleKey is the identity of an SCM entry for push rules, "host/org", both
// canonical, or an error saying what cannot be one entry.
func PushRuleKey(provider, host, org string) (string, error) {
	h, o, err := canonicalPushPair(provider, host, org)
	if err != nil {
		return "", err
	}
	return h + "/" + o, nil
}

// PushRuleEntryOfRemote is how the git route finds the entry a remote belongs
// to, from the remote's own address and never from a request body. It returns
// the CANONICAL host and organisation (the pair PushRuleKey files rules under).
// The host is the remote's host with the ssh. alias of a public host folded; the
// organisation is:
//
//   - GitHub and GitHub Enterprise Server: the first path segment
//     (https://host/org/repo.git, git@host:org/repo.git, ssh://git@host/org/repo).
//   - Azure DevOps: https://dev.azure.com/org/project/_git/repo (a user@ prefix
//     is ignored), https://org.visualstudio.com/project/_git/repo (host becomes
//     dev.azure.com), and, on the SSH hosts only, git@ssh.dev.azure.com:v3/org/project/repo
//     and git@vs-ssh.visualstudio.com:v3/org/project/repo.
//   - Azure DevOps Server (any other host): the collection, in
//     [basePath/]Collection[/Project]/_git/Repo. Without the provider row's base
//     path only Collection/Project/_git/Repo is read; a server under a prefix
//     such as /tfs needs it (PushRuleEntryOfRemoteAt), or the remote is an error,
//     never every collection under one entry.
//
// A path segment that is empty, "." or "..", or that holds an encoded "/", is
// refused: git and curl resolve them, so the push would go somewhere else than
// the entry derived here. A remote whose entry cannot be derived is an error,
// which the caller must treat as a refusal and never as "no rules".
func PushRuleEntryOfRemote(provider, remote string) (host, org string, err error) {
	return PushRuleEntryOfRemoteAt(provider, remote, "")
}

// PushRuleEntryOfRemoteAt is PushRuleEntryOfRemote for a provider row whose
// Azure DevOps Server lives under basePath (for example "/tfs").
func PushRuleEntryOfRemoteAt(provider, remote, basePath string) (host, org string, err error) {
	h, path, err := splitRemote(remote)
	if err != nil {
		return "", "", err
	}
	segs, err := remoteSegments(remote, path)
	if err != nil {
		return "", "", err
	}
	org = segs[0]
	if provider == string(GitProviderAzureDevOps) {
		switch {
		case h == "ssh.dev.azure.com" || h == azureLegacySSHHost:
			if len(segs) < 2 || segs[0] != "v3" {
				return "", "", fmt.Errorf("remote %q has no v3/{organisation} path", remote)
			}
			org = segs[1]
		case h == PublicAzureDevOpsHost:
			// the organisation is the first segment
		case strings.HasSuffix(h, azureLegacySuffix):
			org = strings.TrimSuffix(h, azureLegacySuffix)
		default:
			if org, err = azureServerCollection(remote, segs, basePath); err != nil {
				return "", "", err
			}
		}
	}
	host, org, err = canonicalPushPair(provider, h, org)
	if err != nil {
		return "", "", fmt.Errorf("remote %q: %w", remote, err)
	}
	return host, org, nil
}

// azureServerCollection reads the collection out of an Azure DevOps Server path,
// whose layout is [basePath/]Collection[/Project]/_git/Repo.
func azureServerCollection(remote string, segs []string, basePath string) (string, error) {
	var base []string
	if bp := strings.Trim(basePath, "/"); bp != "" {
		base = strings.Split(bp, "/")
	}
	gi := -1
	for i, s := range segs {
		if s == "_git" {
			gi = i
			break
		}
	}
	// With a base path, the collection follows it and _git is one or two segments
	// later. Without, the only layout read is Collection/Project/_git.
	okGit := gi == len(base)+2 || (len(base) > 0 && gi == len(base)+1)
	if !okGit || len(segs) < gi+2 {
		return "", fmt.Errorf("remote %q: cannot tell which collection it is in (an Azure DevOps Server under a path prefix needs the provider's base path)", remote)
	}
	for i, b := range base {
		if !strings.EqualFold(segs[i], b) {
			return "", fmt.Errorf("remote %q is not under the Azure DevOps Server base path %q", remote, basePath)
		}
	}
	return segs[len(base)], nil
}

// remoteSegments splits a remote's path into segments, refusing any that git or
// curl would resolve to somewhere else: empty ones (// inside the path), "." and
// "..", and anything that decodes to one or holds an encoded "/". One leading
// and one trailing "/" are the path's own.
func remoteSegments(remote, path string) ([]string, error) {
	segs := strings.Split(strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/"), "/")
	for _, s := range segs {
		d := s
		if strings.Contains(s, "%") {
			var err error
			if d, err = url.PathUnescape(s); err != nil {
				return nil, fmt.Errorf("remote %q has a malformed escape in its path", remote)
			}
		}
		if d == "" || d == "." || d == ".." || strings.Contains(d, "/") {
			return nil, fmt.Errorf("remote %q has an empty, dot or encoded-slash path segment", remote)
		}
	}
	return segs, nil
}

// splitRemote returns the lower-case host (trailing dot dropped, aliases NOT
// folded: the caller needs to know the SSH host) and path of a remote in URL or
// scp-like form. An scp-like remote is split at its FIRST ':', as git does, so
// user@host:path cannot smuggle a second host into the path. A host outside
// ASCII is refused before it is lower-cased.
func splitRemote(remote string) (host, path string, err error) {
	var raw string
	if strings.Contains(remote, "://") {
		u, perr := url.Parse(remote)
		if perr != nil {
			return "", "", fmt.Errorf("remote %q is not a URL", remote)
		}
		// git sends ssh:// and git:// paths literally, "?" and "#" included, so what
		// url.Parse moves into the query or fragment (and a ".." after it) is part
		// of the path git resolves.
		if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return "", "", fmt.Errorf("remote %q has a query or fragment", remote)
		}
		raw, path = u.Hostname(), u.EscapedPath()
	} else {
		hostPart, p, ok := strings.Cut(remote, ":")
		if !ok {
			return "", "", fmt.Errorf("remote %q is neither a URL nor user@host:path", remote)
		}
		if strings.Contains(p, ":") {
			return "", "", fmt.Errorf("remote %q has a second ':' in its path", remote)
		}
		raw, path = hostPart[strings.LastIndex(hostPart, "@")+1:], p
	}
	for _, r := range raw {
		if r > unicode.MaxASCII {
			return "", "", fmt.Errorf("remote %q has a host outside ASCII", remote)
		}
	}
	host = strings.TrimSuffix(strings.ToLower(raw), ".")
	if host == "" || strings.Trim(path, "/") == "" {
		return "", "", fmt.Errorf("remote %q has no host or path", remote)
	}
	return host, path, nil
}
