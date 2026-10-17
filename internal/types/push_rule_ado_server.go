// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"strings"
)

// PushRuleEntryOfServerRemote is PushRuleEntryOfRemoteAt for the callers that
// have the provider row's base path in hand, and the one both the run's dispatch
// plan and the git route use, so the two can never key one remote differently.
// Everything but an Azure DevOps Server remote is PushRuleEntryOfRemoteAt's.
//
// An Azure DevOps Server remote is read from its "_git" segment, never by
// counting from the front: the project is the segment right before "_git", and the
// segments between the row's base path and the project are the collection. After
// the base path exactly one such segment may remain when the base path itself
// names no collection, and none when it does (the collection is then the base
// path's last segment). Any other shape is an error, never a guess, because a
// guess files the push under another collection's rules.
//
// A base path of two segments names its collection (prefix and collection). Residual:
// a base path of one segment is a collection or a prefix such as "/tfs", and the
// row does not say which, so the shape of the remote decides: a collection segment
// beyond it makes the base a prefix, none makes it the collection. A collection-only
// row therefore cannot tell a remote with one segment too many from a prefix row's.
func PushRuleEntryOfServerRemote(provider, remote, basePath string) (host, org string, err error) {
	h, path, serr := splitRemote(remote)
	if serr != nil || provider != string(GitProviderAzureDevOps) || adoHostedPushHost(h) {
		return PushRuleEntryOfRemoteAt(provider, remote, basePath)
	}
	segs, err := remoteSegments(remote, path)
	if err != nil {
		return "", "", err
	}
	segs, base, err := serverPathNames(remote, segs, basePath)
	if err != nil {
		return "", "", err
	}
	if len(segs) < len(base) {
		return "", "", fmt.Errorf("remote %q is not under the Azure DevOps Server base path %q", remote, basePath)
	}
	for i, b := range base {
		if !strings.EqualFold(segs[i], b) {
			return "", "", fmt.Errorf("remote %q is not under the Azure DevOps Server base path %q", remote, basePath)
		}
	}
	rest := segs[len(base):]
	gi := -1
	for i, s := range rest {
		if s == "_git" {
			gi = i
			break
		}
	}
	if gi < 1 || len(rest) < gi+2 {
		return "", "", fmt.Errorf("remote %q has no project segment before _git and a repository after it", remote)
	}
	switch before := rest[:gi-1]; {
	case len(before) == 1 && len(base) < 2:
		org = before[0]
	case len(before) == 0 && len(base) > 0:
		org = base[len(base)-1]
	default:
		return "", "", fmt.Errorf("remote %q: cannot tell which collection it is in (%d segments between the base path %q and the project)", remote, len(before), basePath)
	}
	host, org, err = canonicalPushPair(provider, h, org)
	if err != nil {
		return "", "", fmt.Errorf("remote %q: %w", remote, err)
	}
	return host, org, nil
}

// PushRulesForServerRemote is PushRulesForRemoteAt over PushRuleEntryOfServerRemote.
func PushRulesForServerRemote(sets []PushRuleSet, provider, remote, basePath string) (spec PushRulesSpec, found bool, err error) {
	host, org, err := PushRuleEntryOfServerRemote(provider, remote, basePath)
	if err != nil {
		return PushRulesSpec{}, false, err
	}
	key, err := PushRuleKey(provider, host, org)
	if err != nil {
		return PushRulesSpec{}, false, err
	}
	for _, s := range sets {
		if s.Key() == key {
			return s.Rules, true, nil
		}
	}
	return PushRulesSpec{}, false, nil
}

// adoHostedPushHost reports a host Azure DevOps Services answers on.
func adoHostedPushHost(h string) bool {
	return h == PublicAzureDevOpsHost || h == "ssh.dev.azure.com" || h == azureLegacySSHHost || strings.HasSuffix(h, azureLegacySuffix)
}
