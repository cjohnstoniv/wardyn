// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeAuth(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestAuthFileEntryShapesAndHostForms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeAuth(t, p, `{"auths":{
	  "reg.corp.example":{"auth":"`+b64("alice:s3cret")+`"},
	  "https://ghcr.io":{"username":"bob","password":"pw"},
	  "registry.local:5000":{"identitytoken":"idtok"},
	  "https://index.docker.io/v1/":{"auth":"`+b64("dh:pw2")+`"},
	  "other.example:443":{"auth":"`+b64("o:o")+`"}
	}}`, time.Now())
	a := NewAuthFile(p)
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host string
		want RegistryAuth
	}{
		{"reg.corp.example", RegistryAuth{Username: "alice", Password: "s3cret"}},
		{"REG.corp.example", RegistryAuth{Username: "alice", Password: "s3cret"}},
		{"ghcr.io", RegistryAuth{Username: "bob", Password: "pw"}},
		{"registry.local:5000", RegistryAuth{IdentityToken: "idtok"}},
		{"registry-1.docker.io", RegistryAuth{Username: "dh", Password: "pw2"}},
		{"other.example", RegistryAuth{Username: "o", Password: "o"}},
	}
	for _, tc := range cases {
		got, ok, err := a.Lookup(tc.host)
		if err != nil || !ok || got != tc.want {
			t.Errorf("Lookup(%q) = %+v, %v, %v; want %+v", tc.host, got, ok, err, tc.want)
		}
	}
	if _, ok, _ := a.Lookup("reg.corp.example:8443"); ok {
		t.Error("a credential for reg.corp.example matched another port")
	}
	if _, ok, _ := a.Lookup("evil.reg.corp.example"); ok {
		t.Error("a credential matched a subdomain of its host")
	}
}

func TestAuthFileRefusesCredentialHelpers(t *testing.T) {
	for name, body := range map[string]string{
		"credsStore":  `{"credsStore":"desktop","auths":{"h":{"auth":"` + b64("u:topsecretpw") + `"}}}`,
		"credHelpers": `{"credHelpers":{"gcr.io":"gcloud"},"auths":{"h":{"auth":"` + b64("u:topsecretpw") + `"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.json")
			writeAuth(t, p, body, time.Now())
			a := NewAuthFile(p)
			err := a.Validate()
			if !errors.Is(err, ErrCredentialHelper) {
				t.Fatalf("Validate = %v, want ErrCredentialHelper", err)
			}
			if _, _, err := a.Lookup("h"); !errors.Is(err, ErrCredentialHelper) {
				t.Fatalf("Lookup = %v, want ErrCredentialHelper", err)
			}
			if strings.Contains(err.Error(), "topsecretpw") || strings.Contains(err.Error(), b64("u:topsecretpw")) {
				t.Errorf("error leaks the credential: %v", err)
			}
		})
	}
}

func TestAuthFileRotationIsPickedUp(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	t0 := time.Now().Add(-time.Hour)
	writeAuth(t, p, `{"auths":{"h.example":{"auth":"`+b64("old:old")+`"}}}`, t0)
	a := NewAuthFile(p)
	if got, _, _ := a.Lookup("h.example"); got.Username != "old" {
		t.Fatalf("first read = %+v", got)
	}
	// Same size, new mtime: a rotated Secret mount.
	writeAuth(t, p, `{"auths":{"h.example":{"auth":"`+b64("new:new")+`"}}}`, t0.Add(time.Minute))
	if got, _, _ := a.Lookup("h.example"); got.Username != "new" {
		t.Fatalf("after rotation = %+v, want the new credential", got)
	}
}

func TestAuthFileUnchangedIsNotReread(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	t0 := time.Now().Add(-time.Hour)
	writeAuth(t, p, `{"auths":{"h.example":{"auth":"`+b64("a:a")+`"}}}`, t0)
	a := NewAuthFile(p)
	_, _, _ = a.Lookup("h.example")
	// Same size and mtime but different bytes: only a re-read would see it.
	writeAuth(t, p, `{"auths":{"h.example":{"auth":"`+b64("b:b")+`"}}}`, t0)
	if got, _, _ := a.Lookup("h.example"); got.Username != "a" {
		t.Fatalf("file with unchanged mtime and size was re-read: %+v", got)
	}
}

func TestAuthFileUnsetIsAnonymousAndMissingIsAnError(t *testing.T) {
	if _, ok, err := NewAuthFile("").Lookup("h"); ok || err != nil {
		t.Fatalf("unset path: ok=%v err=%v, want anonymous", ok, err)
	}
	if err := NewAuthFile("").Validate(); err != nil {
		t.Fatalf("unset path Validate = %v", err)
	}
	if err := NewAuthFile(filepath.Join(t.TempDir(), "absent.json")).Validate(); err == nil {
		t.Fatal("a named but absent file validated")
	}
}

func TestAuthFileBadJSONDoesNotLeak(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeAuth(t, p, `{"auths":{"h":{"auth":"`+b64("u:leakme")+`"` /* truncated */, time.Now())
	err := NewAuthFile(p).Validate()
	if err == nil || strings.Contains(err.Error(), "leakme") {
		t.Fatalf("err = %v", err)
	}
}
