// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// The restore image mirror resolves flag, then the profile's MacOSVMImageURL / MacOSVMImageHash, then
// runnerd.json; the file refuses a mirror that is not https, has a credential, or has no hash.
func TestMacOSVMImageMirrorSetting(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	file := runnerFile{MacOSVMImageURL: "https://file.example/a.ipsw", MacOSVMImageHash: hash}
	cfg := resolveRunnerConfig(inputs(nil, nil, map[string]string{"MacOSVMImageURL": "https://managed.example/a.ipsw"}, file))
	if got := cfg.field("macos_vm_image_url"); got.Value != "https://managed.example/a.ipsw" || !strings.Contains(got.Source, "managed preference MacOSVMImageURL") {
		t.Fatalf("the profile lost to the file: %+v", got)
	}
	cfg = resolveRunnerConfig(inputs(map[string]string{"ipsw-url": "https://flag.example/a.ipsw"}, nil, map[string]string{"MacOSVMImageURL": "https://managed.example/a.ipsw"}, file))
	if got := cfg.get("macos_vm_image_url"); got != "https://flag.example/a.ipsw" {
		t.Fatalf("the flag lost to the profile: %q", got)
	}
	if got := resolveRunnerConfig(inputs(nil, nil, nil, file)).get("macos_vm_image_sha256"); got != hash {
		t.Fatalf("runnerd.json's hash = %q", got)
	}

	for name, f := range map[string]runnerFile{
		"http":        {MacOSVMImageURL: "http://mirror.example/a.ipsw", MacOSVMImageHash: hash},
		"credential":  {MacOSVMImageURL: "https://u:p@mirror.example/a.ipsw", MacOSVMImageHash: hash},
		"no hash":     {MacOSVMImageURL: "https://mirror.example/a.ipsw"},
		"short hash":  {MacOSVMImageURL: "https://mirror.example/a.ipsw", MacOSVMImageHash: hash[:63]},
		"hash no url": {MacOSVMImageHash: hash},
	} {
		if err := checkRunnerFile(f); err == nil || !strings.Contains(err.Error(), "macos_vm_image") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := checkRunnerFile(file); err != nil {
		t.Fatalf("a good mirror: %v", err)
	}
}
