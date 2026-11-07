// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The control plane is given an image admitter, so a site that turns images.admission on has its images
// checked rather than every governed launch refused with image_admission_unavailable.
func TestImageAdmitterIsWired(t *testing.T) {
	a, err := newImageAdmitter(func(string) string { return "" })
	if err != nil || a == nil {
		t.Fatalf("newImageAdmitter = %v, %v; want an admitter", a, err)
	}
	adm := &types.SiteImageAdmission{Mode: types.ImageAdmissionEnforce, Signers: []types.ImageSigner{{
		Name: "corp-ci", Images: []string{"registry.corp.example/team/*"},
		Keyless: &types.ImageKeyless{Issuer: "https://token.actions.githubusercontent.com",
			Identity: "https://github.com/corp/images/.github/workflows/build.yml@refs/heads/main"}}}}
	req, refused, err := a.Require(adm, "registry.corp.example/team/app:1.4", true, nil)
	if err != nil || refused != nil || len(req.AnyOf) == 0 {
		t.Fatalf("covered image: requirements %+v, refusal %+v, error %v; want the site signer", req, refused, err)
	}
	req, refused, err = a.Require(adm, "other.example/app:1", true, nil)
	if err != nil || refused == nil || refused.Reason != imageadmit.NoPolicy {
		t.Fatalf("uncovered image: refusal %+v, error %v; want no_policy", refused, err)
	}
	if v, err := a.Admit(context.Background(), req); err != nil || v.Reason != imageadmit.NoPolicy {
		t.Fatalf("uncovered image Admit = %+v, %v; want no_policy with no registry read", v, err)
	}

	// run() hands the admitter to the API server.
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wired := false
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || selectorName(cl.Type) != "api.Config" {
			return true
		}
		for _, e := range cl.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "ImageAdmissionConfig" {
					wired = true
				}
			}
		}
		return true
	})
	if !wired {
		t.Fatal("main.go builds api.Config without ImageAdmissionConfig: image admission would refuse every checked launch")
	}
}

// selectorName is "pkg.Name" for a qualified type expression, else "".
func selectorName(e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return x.Name + "." + sel.Sel.Name
}
