// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"
)

// The storage note is never a credential and never another scheme: only an http or https link to a host is
// stored, and one with a user name, a password, a query or a fragment is refused, and so is a form whose parts
// are not split out (an opaque https:rest), which could hide any of them.
func TestOrgSiteStorageURLRefusesUserinfoAndQuery(t *testing.T) {
	for _, ok := range []string{
		"",
		"https://files.example.com/share",
		"https://files.example.com:8443/share/designs",
		"HTTPS://files.example.com/share",
		"http://nas.office.example/designs",
	} {
		if err := ValidateOrgSiteStorage("Office NAS", ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"https://alice@files.example.com/share",
		"https://alice:secret@files.example.com/share",
		"s3://AKIAEXAMPLE:secret@designs-bucket/prefix",
		"https://files.example.com/share?sig=abc",
		"https://files.example.com/share?",
		"smb:alice:secret@nas.office.example/designs",
		"https:alice:secret@files.example.com/share",
		"javascript:alert(1)",
		"javascript://x/%0aalert(1)",
		"data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==",
		"https://files.example.com/share#token=abc",
		"https://files.example.com/share#",
		"https:///share",
		"smb://nas.office.example/designs",
		"s3://designs-bucket/prefix",
		"file:///mnt/designs",
		"files.example.com/share",
		"/mnt/designs",
		" https://files.example.com/share",
		"https://files.example.com/share\nx",
		"https://files.example.com/" + strings.Repeat("a", OrgSiteStorageURLMax),
	} {
		if err := ValidateOrgSiteStorage("", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, bad := range []string{" NAS", "NAS\tshare", strings.Repeat("n", OrgSiteStorageLabelMax+1)} {
		if err := ValidateOrgSiteStorage(bad, ""); err == nil {
			t.Errorf("label %q accepted", bad)
		}
	}
	if err := (OrgSite{Name: "Office", StorageURL: "https://u:p@files.example.com"}).Validate(); err == nil {
		t.Error("Validate passed a link with a password")
	}
}

func TestValidateOrgSiteName(t *testing.T) {
	for _, ok := range []string{"Office", "Edge cluster 3", "Région Est", strings.Repeat("s", OrgSiteNameMax)} {
		if err := ValidateOrgSiteName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", " Office", "Office ", "Off\nice", strings.Repeat("s", OrgSiteNameMax+1)} {
		if err := ValidateOrgSiteName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if !OrgSiteActive.Valid() || !OrgSiteDeleted.Valid() || OrgSiteState("disabled").Valid() {
		t.Error("the site states are active and deleted only")
	}
}

// site_default is a selection the run row stores (the migration's CHECK admits it) and Valid accepts.
func TestRunnerPoolSelectionValid(t *testing.T) {
	for _, s := range []RunnerPoolSelection{RunnerPoolSelectedExplicit, RunnerPoolSelectedPersonal, RunnerPoolSelectedOrg, RunnerPoolSelectedSite} {
		if !s.Valid() {
			t.Errorf("%q is not valid", s)
		}
	}
	if RunnerPoolSelectedSite != "site_default" {
		t.Errorf("the site selection's wire value is %q", RunnerPoolSelectedSite)
	}
	for _, s := range []RunnerPoolSelection{"", "site", "organization_default"} {
		if s.Valid() {
			t.Errorf("%q is valid", s)
		}
	}
}
