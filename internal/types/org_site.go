// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// An organisation site is an admin's label for a place, such as an office, an edge cluster or a cloud region,
// over organization runner pools and admin drives. It grants nothing and isolates nothing: a run whose drives
// are homed at a site may be placed in the site's default pool, and the run and its audit row name the site.
// A runner never claims a site; only an admin assigns a pool to one. The console and docs call it a "Site";
// the code says OrgSite, because the install-wide settings already use the Site* names (SiteConfig).

// OrgSiteState is a site's lifecycle. A deleted site is a tombstone: its row stays, so a run placed at it
// keeps its record, and nothing new can be assigned to it.
type OrgSiteState string

const (
	OrgSiteActive  OrgSiteState = "active"
	OrgSiteDeleted OrgSiteState = "deleted"
)

// Valid reports whether s is one of the two.
func (s OrgSiteState) Valid() bool { return s == OrgSiteActive || s == OrgSiteDeleted }

// OrgSiteNameMax is the longest site name, in characters, the same bound as a pool name's.
const OrgSiteNameMax = RunnerPoolNameMax

// OrgSiteStorageLabelMax and OrgSiteStorageURLMax bound the storage note, in characters.
const (
	OrgSiteStorageLabelMax = 128
	OrgSiteStorageURLMax   = 2048
)

// OrgSite is one site. DefaultPoolID, when set, is one of the site's own pools. StorageLabel and StorageURL
// are a note for people: Wardyn never connects to the URL and stores no credential for it, and the URL never
// enters an audit row. Revision starts at 1 and rises by one on every change to the site's row.
type OrgSite struct {
	ID            uuid.UUID    `json:"id"`
	Name          string       `json:"name"`
	State         OrgSiteState `json:"state"`
	DefaultPoolID *uuid.UUID   `json:"default_pool_id,omitempty"`
	StorageLabel  string       `json:"storage_label,omitempty"`
	StorageURL    string       `json:"storage_url,omitempty"`
	// PoolIDs are the pools assigned to the site, by id. A read fills it; a write ignores it.
	PoolIDs   []uuid.UUID `json:"pool_ids"`
	Revision  int64       `json:"revision"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// Validate checks the fields an admin writes: the name and the storage note.
func (s OrgSite) Validate() error {
	if err := ValidateOrgSiteName(s.Name); err != nil {
		return err
	}
	return ValidateOrgSiteStorage(s.StorageLabel, s.StorageURL)
}

// OrgSiteDrive is one admin drive homed at a live site.
type OrgSiteDrive struct {
	DriveID   uuid.UUID `json:"drive_id"`
	DriveName string    `json:"drive_name"`
	SiteID    uuid.UUID `json:"site_id"`
	SiteName  string    `json:"site_name"`
}

// ValidateOrgSiteName accepts a printable, trimmed, single-line name of 1 to OrgSiteNameMax characters, the
// rule a pool name follows.
func ValidateOrgSiteName(name string) error {
	if name == "" || name != strings.TrimSpace(name) {
		return fmt.Errorf("name a site in 1 to %d characters, without leading or trailing spaces", OrgSiteNameMax)
	}
	if !printableLine(name) {
		return errors.New("a site name is one printable line")
	}
	if n := len([]rune(name)); n > OrgSiteNameMax {
		return fmt.Errorf("a site name is at most %d characters", OrgSiteNameMax)
	}
	return nil
}

// ValidateOrgSiteStorage checks a site's storage note. Both parts are optional. The label is one printable,
// trimmed line. The URL is an http or https link to a host, and carries no user name, password, query or
// fragment, so no credential can be written into it and no other scheme reaches a page that shows it; an
// opaque form (https:rest) is refused too, since its rest is not split into those parts.
func ValidateOrgSiteStorage(label, rawURL string) error {
	if label != strings.TrimSpace(label) || !printableLine(label) {
		return errors.New("a storage label is one printable line, without leading or trailing spaces")
	}
	if len([]rune(label)) > OrgSiteStorageLabelMax {
		return fmt.Errorf("a storage label is at most %d characters", OrgSiteStorageLabelMax)
	}
	if rawURL == "" {
		return nil
	}
	if rawURL != strings.TrimSpace(rawURL) || !printableLine(rawURL) || len([]rune(rawURL)) > OrgSiteStorageURLMax {
		return fmt.Errorf("a storage link is one printable line of at most %d characters", OrgSiteStorageURLMax)
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Opaque != "" || u.Host == "" {
		return errors.New("a storage link is an http or https URL, such as https://files.example.com/share")
	}
	if u.User != nil {
		return errors.New("a storage link carries no user name or password")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return errors.New("a storage link carries no query")
	}
	if strings.ContainsRune(rawURL, '#') {
		return errors.New("a storage link carries no fragment")
	}
	return nil
}

func printableLine(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
