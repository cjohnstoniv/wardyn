// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The organisation site reasons: the same closed set and convention as reasons.go, in a file of their own beside
// org_sites.go. TestReasonDocsMatchReasonsGo reads this file too.
const (
	reasonOrgSitesOff        = "org_sites_off"         // features.org_sites is off: a write that adds a site, a pool at one or a drive's home is refused; removals are not
	reasonOrgSiteNotFound    = "org_site_not_found"    // no site has that id, or it is deleted
	reasonOrgSiteInvalid     = "org_site_invalid"      // the name or the storage note fails the site rules
	reasonOrgSiteNameTaken   = "org_site_name_taken"   // a live site already has that name
	reasonOrgSiteStale       = "org_site_stale"        // the revision sent is not the site's
	reasonOrgSiteInUse       = "org_site_in_use"       // a pool or a drive is still at the site, so it is not deleted
	reasonOrgSitePoolScope   = "org_site_pool_scope"   // only an organization pool, and only an admin drive, can be at a site
	reasonOrgSiteDefaultPool = "org_site_default_pool" // a site's default pool is one of its own live pools
)
