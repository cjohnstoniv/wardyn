// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// ImageBuildSchema is the record version; a higher number reads as unrecorded.
const ImageBuildSchema = 1

// Build lanes and the reasons a build ran, as the record names them.
const (
	ImageBuildLaneGenerated = "generated"
	ImageBuildLaneBYOI      = "byoi"
	ImageBuildLaneRepo      = "repo_devcontainer"

	ImageBuildReasonCacheMiss    = "cache_miss"
	ImageBuildReasonImageMissing = "image_missing"
	ImageBuildReasonRefresh      = "refresh"
)

// ImageBuild is the record of what built a workspace's image. An empty string
// means "not known"; a field is never guessed.
type ImageBuild struct {
	Schema        int              `json:"schema"`
	Lane          string           `json:"lane"`
	CacheKey      string           `json:"cache_key"`
	BuildID       string           `json:"build_id"`
	Reason        string           `json:"reason"`
	BuiltAt       time.Time        `json:"built_at"`
	WardynVersion string           `json:"wardyn_version"`
	Resolved      bool             `json:"resolved"`
	Inputs        ImageBuildInputs `json:"inputs"`
	Image         ImageBuildImage  `json:"image"`
}

// ImageBuildInputs are the build's unpinned-or-pinned inputs, per lane.
type ImageBuildInputs struct {
	Base       ImageBuildRef         `json:"base"`
	Features   []ImageBuildFeature   `json:"features,omitempty"`
	Installers []ImageBuildInstaller `json:"installers,omitempty"`
	Generator  *ImageBuildGenerator  `json:"generator,omitempty"`
	Repo       *ImageBuildRepo       `json:"repo,omitempty"`
}

// ImageBuildRef is an image reference and the digest it resolved to.
type ImageBuildRef struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// ImageBuildFeature is a dev container feature; Options is what the generator wrote.
type ImageBuildFeature struct {
	Ref     string         `json:"ref"`
	Digest  string         `json:"digest"`
	Options map[string]any `json:"options"`
}

// ImageBuildInstaller is a tool the generated Dockerfile installs.
type ImageBuildInstaller struct {
	Tool     string `json:"tool"`
	Channel  string `json:"channel"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	SHA256   string `json:"sha256"`
}

// ImageBuildGenerator names the generator salt and the hash of each file it built.
type ImageBuildGenerator struct {
	Salt        string            `json:"salt"`
	FilesSHA256 map[string]string `json:"files_sha256"`
}

// ImageBuildRepo is the repository a repo-devcontainer build read, as built.
type ImageBuildRepo struct {
	URL string `json:"url"`
	Ref string `json:"ref"`
}

// ImageBuildImage is the build's own tag and its image ID.
type ImageBuildImage struct {
	Ref string `json:"ref"`
	ID  string `json:"id"`
}

// SHA256 is the hex SHA-256 of the record's encoding/json bytes. It is
// computed, never stored.
func (r ImageBuild) SHA256() (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
