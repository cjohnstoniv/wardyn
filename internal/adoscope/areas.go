// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

// readArea is what a read of one area needs: the capability a run must hold,
// and the scope a token must carry to perform it.
type readArea struct {
	cap   Capability
	scope string
}

// readAreas is EVERY area this catalogue answers a read for, with the read
// capability and the scope it needs. CapDiscovery's areas carry no scope: the
// service doesn't gate them. A key "area/resource" is a resource whose read
// differs from its area's. CLOSED table: a read outside it is
// CapUnclassifiedRead (not grantable) rather than 403ing at the forge. The
// read capabilities' scopes in scopes.go are DERIVED from this table.
var readAreas = map[string]readArea{
	"git":    {CapCodeRead, "vso.code"},
	"policy": {CapCodeRead, "vso.code"},
	// Search is three documented resources on its own host, one per area;
	// any other search resource is an unclassified read.
	"search/codesearchresults":     {CapCodeRead, "vso.code"},
	"search/workitemsearchresults": {CapWorkRead, "vso.work"},
	"search/wikisearchresults":     {CapWikiRead, "vso.wiki"},

	"wit":  {CapWorkRead, "vso.work"},
	"work": {CapWorkRead, "vso.work"},

	"wiki": {CapWikiRead, "vso.wiki"},

	"build":     {CapBuildRead, "vso.build"},
	"pipelines": {CapBuildRead, "vso.build"},
	"release":   {CapReleaseRead, "vso.release"},

	"serviceendpoint":                {CapServiceEndpointRead, "vso.serviceendpoint"},
	"distributedtask/variablegroups": {CapLibraryRead, "vso.variablegroups_read"},
	"distributedtask/securefiles":    {CapLibraryRead, "vso.securefiles_read"},

	"packaging": {CapPackagingRead, "vso.packaging"},
	"packages":  {CapPackagingRead, "vso.packaging"},

	"test":        {CapTestRead, "vso.test"},
	"testplan":    {CapTestRead, "vso.test"},
	"testresults": {CapTestRead, "vso.test"},

	"projects":           {CapProjectRead, "vso.project"},
	"projectcollections": {CapProjectRead, "vso.project"},
	// The signed-in person's profile and organisation list.
	"profile":  {CapProjectRead, "vso.profile"},
	"accounts": {CapProjectRead, "vso.profile"},

	"graph":              {CapIdentityRead, "vso.graph"},
	"identities":         {CapIdentityRead, "vso.identity"},
	"userentitlements":   {CapIdentityRead, "vso.memberentitlementmanagement"},
	"groupentitlements":  {CapIdentityRead, "vso.memberentitlementmanagement"},
	"memberentitlements": {CapIdentityRead, "vso.memberentitlementmanagement"},

	"analytics": {CapAnalyticsRead, "vso.analytics"},

	"connectiondata": {CapDiscovery, ""},
	"resourceareas":  {CapDiscovery, ""},
}

// readAreaOf is the read area r falls in, and whether r is a read this
// catalogue knows at all.
func readAreaOf(r route) (readArea, bool) {
	// A package client's feed route carries no _apis segment, but it is the
	// packaging area all the same.
	if packagePublish(r) {
		return readAreas["packaging"], true
	}
	if a, ok := readAreas[r.area+"/"+r.res]; ok {
		return a, true
	}
	a, ok := readAreas[r.area]
	return a, ok
}
