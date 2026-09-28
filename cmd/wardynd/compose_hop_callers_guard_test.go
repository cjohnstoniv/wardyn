// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// Once the hop is TLS the console listener refuses /api/v1/internal/*
// (#1263), so on the compose stack every internal caller must dial the URL
// wardynd itself boots its internal listener for. wardynd's side is resolved
// by the real parseBootFlags from its compose environment; every other service
// naming WARDYN_CONTROL_PLANE_URL (the ground-truth ingest) must name the same
// URL. Helm's side is the Makefile helm-lint render check.
func TestComposeInternalCallersDialTheInternalListener(t *testing.T) {
	files, err := filepath.Glob("../../deploy/compose/docker-compose*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no compose files: %v", err)
	}
	type service struct {
		Environment map[string]string `yaml:"environment"`
	}
	callers := map[string]string{}
	var wardynd map[string]string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Services map[string]service `yaml:"services"`
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for name, svc := range doc.Services {
			if name == "wardynd" && filepath.Base(f) == "docker-compose.yaml" {
				wardynd = svc.Environment
				continue
			}
			if u, ok := svc.Environment["WARDYN_CONTROL_PLANE_URL"]; ok {
				callers[filepath.Base(f)+" "+name] = u
			}
		}
	}
	if wardynd == nil || len(callers) == 0 {
		t.Fatalf("compose no longer has a wardynd service (%v) or any other internal caller (%v): re-point this guard", wardynd != nil, callers)
	}

	resetFlags(t)
	oldArgs := os.Args
	os.Args = []string{"wardynd-test"}
	t.Cleanup(func() { os.Args = oldArgs })
	for _, k := range []string{"WARDYN_LISTEN", "WARDYN_CONTROL_PLANE_URL", "WARDYN_INTERNAL_LISTEN"} {
		if v, ok := wardynd[k]; ok {
			t.Setenv(k, v)
		} else {
			ensureUnset(t, k)
		}
	}
	f := parseBootFlags()
	u, err := url.Parse(*f.controlURL)
	if err != nil || u.Scheme != "https" {
		t.Fatalf("compose wardynd WARDYN_CONTROL_PLANE_URL %q: want https (hop TLS on)", *f.controlURL)
	}
	_, internalPort, _ := net.SplitHostPort(*f.internalListen)
	_, consolePort, _ := net.SplitHostPort(*f.listen)
	if u.Port() != internalPort || u.Port() == consolePort {
		t.Fatalf("compose wardynd dispatches proxies to port %s; internal listener %q, console %q", u.Port(), *f.internalListen, *f.listen)
	}
	for svc, got := range callers {
		if got != *f.controlURL {
			t.Errorf("%s: WARDYN_CONTROL_PLANE_URL %q, want wardynd's internal listener %q (the console refuses /api/v1/internal/*)", svc, got, *f.controlURL)
		}
	}
}
