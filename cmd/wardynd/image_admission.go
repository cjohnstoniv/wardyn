// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// registryAuthFileEnv names a Docker config.json-shaped file of registry credentials for image admission's
// registry reads. Unset means anonymous reads. A file, never a value, keeps the credential out of the
// process environment.
const registryAuthFileEnv = "WARDYN_REGISTRY_AUTH_FILE"

// newImageRegistry is the registry client image admission reads through. The auth file is read now: one
// that cannot be read or parsed, or that names a credential helper (wardynd never runs one), refuses to
// start rather than failing every later check; the client re-reads it on change after that. The transport
// is http.DefaultTransport as installBootTransport left it, so a registry read keeps WARDYN_TRUSTED_CA_FILE's
// roots and the daemon's forward proxy. Building it makes no request: wardynd reads a registry only to check
// a run's image, which happens only while images.admission is on or a governance profile requires a signer.
func newImageRegistry(getenv func(string) string) (*imageadmit.RegistryClient, error) {
	auth := imageadmit.NewAuthFile(getenv(registryAuthFileEnv))
	if err := auth.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", registryAuthFileEnv, err)
	}
	base, _ := http.DefaultTransport.(*http.Transport)
	return imageadmit.NewRegistryClient(imageadmit.RegistryOptions{Base: base, Auth: auth}), nil
}

// newImageAdmitter is the check run create and queue start ask, one per replica, so verdicts are kept per
// replica. It is built whatever the site setting: images.admission is read on each check, and a governance
// profile can require a signer while the site mode is off.
func newImageAdmitter(getenv func(string) string) (api.ImageAdmitter, error) {
	reg, err := newImageRegistry(getenv)
	if err != nil {
		return nil, err
	}
	return imageAdmitter{imageadmit.NewAdmitter(reg, imageadmit.AdmitterOptions{})}, nil
}

// imageAdmitter is api.ImageAdmitter over imageadmit: the site block is compiled for each check.
type imageAdmitter struct{ *imageadmit.Admitter }

func (imageAdmitter) Require(a *types.SiteImageAdmission, ref string, site bool, profile []string) (imageadmit.Requirements, *imageadmit.Verdict, error) {
	p, err := imageadmit.Compile(imageadmit.SiteConfig(a), imageadmit.Options{})
	if err != nil {
		return imageadmit.Requirements{}, nil, err
	}
	req, v := p.Require(ref, site, profile)
	return req, v, nil
}
