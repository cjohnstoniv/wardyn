// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Azure DevOps SERVER on the own-token path. A Server row is lanes ["pat"],
// credential_source per_user and no entra block (#1429): Server has no Entra
// sign-in, so each person adds their own token exactly as on a Services
// own_pat row (ado_own_pat.go), checked, stored and read the same way.
//
// GIT ONLY. A Server run's token rides one door: the proxy's git broker, which
// adds it (Basic) on the way out and holds git to the collection pin, the
// capability check, the run's branch rule and the content rules
// (serveADOGit). No REST route to the server is opened — no TLS-interception
// entry, and the proxy refuses a tunnel to a host the grant covers — and the
// sandbox holds nothing: not the token, and not a grant it could mint one
// with, whether or not the PAT broker is on.
//
// THE COLLECTION IS THE PIN. A Server row's address must name its collection
// (https://host/Collection, or under a virtual directory
// https://host/tfs/Collection — one or two segments): the path is the
// organisation the grant pins git to, and where the identity check asks. A row
// naming only the host, or a deeper path, is not served — a pin of the whole
// server would pin nothing, and the proxy's pin holds two segments at most.

import (
	"net/url"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoServerCapabilities is what a Server run holds. The row has no ceiling to
// read (no entra block), and the lane is git only: read, and push — a push
// still confined to the run's own branch unless its policy allows any branch.
var adoServerCapabilities = []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapCodeWrite}

// adoServerTokenScopes is what a person ticks for a Server token: git only.
var adoServerTokenScopes = []string{"Code (Read & write)"}

// isADOServiceHost reports whether host is Azure DevOps Services'.
func isADOServiceHost(host string) bool {
	h := strings.ToLower(host)
	return h == "dev.azure.com" || strings.HasSuffix(h, ".visualstudio.com")
}

// adoServerAddress is a Server row's first address split into its host and
// collection path (lower case, no surrounding slashes). ok=false when the
// address is not an https Server address naming a collection.
func adoServerAddress(row types.GitProvider) (host, collection string, ok bool) {
	if len(row.BaseURLs) == 0 {
		return "", "", false
	}
	u, err := url.Parse(row.BaseURLs[0])
	if err != nil || u.Scheme != "https" || u.Port() != "" || u.Hostname() == "" || isADOServiceHost(u.Hostname()) {
		return "", "", false
	}
	collection = strings.ToLower(strings.Trim(u.Path, "/"))
	// A collection is one segment, or two under a virtual directory
	// (tfs/DefaultCollection); the proxy's pin holds no more than that.
	segs := strings.Split(collection, "/")
	if collection == "" || len(segs) > 2 || slices.Contains(segs, "") {
		return "", "", false
	}
	return strings.ToLower(u.Hostname()), collection, true
}

// isADOServerOwnPATRow reports whether row is a Server row a person adds their
// own token on: Azure DevOps, the pat lane, per_user, no entra block, and a
// first address naming a collection.
func isADOServerOwnPATRow(row types.GitProvider) bool {
	if row.Kind != types.GitProviderAzureDevOps || row.Entra != nil ||
		row.CredentialSource != types.CredentialSourcePerUser || !slices.Contains(row.Lanes, types.GitLanePAT) {
		return false
	}
	_, _, ok := adoServerAddress(row)
	return ok
}

// isADOOwnTokenRow reports whether row takes each person's own token, on
// Services (token_mode own_pat) or on Server.
func isADOOwnTokenRow(row types.GitProvider) bool {
	return isADOOwnPATRow(row) || isADOServerOwnPATRow(row)
}

// adoServerRunForRepo is adoEntraRunForRepo's Server arm: the run's lane when
// repo is on the row's host under its collection. The organisation is the
// collection path, the pin the git broker holds every request to.
func adoServerRunForRepo(row types.GitProvider, repo, owner string) (adoEntraRun, bool) {
	host, collection, ok := adoServerAddress(row)
	if !ok {
		return adoEntraRun{}, false
	}
	t, ok := parseCloneTarget(repo, []string{host})
	if !ok || t.ssh || t.scheme != "https" || t.host != host ||
		!strings.HasPrefix(strings.ToLower(strings.Trim(t.path, "/"))+"/", collection+"/") {
		return adoEntraRun{}, false
	}
	return adoEntraRun{
		rowID: row.ID, org: collection, owner: owner, serverHost: host,
		tokenMode: types.ADOTokenModeOwnPAT, credentialSource: row.CredentialSource,
		caps: slices.Clone(adoServerCapabilities), ceiling: slices.Clone(adoServerCapabilities),
	}, true
}

// laneHosts is where the run's Azure DevOps credential may ride: the Server
// host alone, or the organisation's Services hosts.
func (a adoEntraRun) laneHosts() []string {
	if a.serverHost != "" {
		return []string{a.serverHost}
	}
	return adoEntraHosts(a.org)
}

// adoServerDrift is driftFrom for a Server run: the name of the first thing
// about the live row that no longer matches the snapshot, "" when none does.
func adoServerDrift(row types.GitProvider, sn adoEntraScopeSnapshot) string {
	_, collection, _ := adoServerAddress(row)
	switch {
	case row.Disabled || !isADOServerOwnPATRow(row):
		return "lane_withdrawn"
	case string(row.CredentialSource) != sn.CredentialSource:
		return "credential_source"
	case types.ADOTokenMode(sn.TokenMode) != types.ADOTokenModeOwnPAT:
		return "token_mode"
	case collection != sn.Organisation:
		return "organisation"
	case !subsetOf(sn.Capabilities, adoServerCapabilities):
		return "capability_ceiling"
	}
	return ""
}

// adoOwnPATTarget is where a row's identity check asks and the organisation a
// stored token is for: the Services organisation's connectionData, or the
// Server collection's. ok=false for a row that is not an own-token row.
func adoOwnPATTarget(row types.GitProvider) (identityURL, org string, ok bool) {
	if host, collection, server := adoServerAddress(row); server && isADOServerOwnPATRow(row) {
		u, _ := url.Parse(row.BaseURLs[0])
		// No api-version: every Server release answers connectionData without
		// one, and a version newer than the server's is refused.
		return "https://" + host + "/" + strings.Trim(u.EscapedPath(), "/") + "/_apis/connectionData", collection, true
	}
	if !isADOOwnPATRow(row) {
		return "", "", false
	}
	org, found := adoOrganisationOf(adoOrgDisplay(row))
	if !found {
		return "", "", false
	}
	// No api-version here either: Services answers connectionData?api-version=7.1
	// with a 400 for every token, and without one with the owner.
	return adoOwnPATAPIBase + "/" + url.PathEscape(org) + "/_apis/connectionData", org, true
}
