// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestUnavailableNamesTheRegistryNotAHostItChose: a redirect's Location and a token realm are chosen by the
// registry, so when the host they name fails, the member's sentence, the error text and the log name the
// registry the image ref named, never that other host.
func TestUnavailableNamesTheRegistryNotAHostItChose(t *testing.T) {
	const marker = "leak-marker"
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	_, closed, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	var srvPort string
	other := func(p string) string { return "https://" + marker + ".example.com:" + p }
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		// The registry redirects to another origin, which sends a challenge.
		"redirect answers 401": func(w http.ResponseWriter, r *http.Request) {
			if r.Host == "example.com:"+srvPort {
				http.Redirect(w, r, other(srvPort)+"/elsewhere", http.StatusFound)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+other(srvPort)+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		},
		// The registry redirects to another origin, which cuts its body short.
		"redirected body read fails": func(w http.ResponseWriter, r *http.Request) {
			if r.Host == "example.com:"+srvPort {
				http.Redirect(w, r, other(srvPort)+"/elsewhere", http.StatusFound)
				return
			}
			w.Header().Set("Content-Length", "4096")
			_, _ = w.Write([]byte("{}"))
		},
		"redirect answers 403": func(w http.ResponseWriter, r *http.Request) {
			if r.Host == "example.com:"+srvPort {
				http.Redirect(w, r, other(srvPort)+"/elsewhere", http.StatusFound)
				return
			}
			w.WriteHeader(http.StatusForbidden)
		},
		// The challenge names a realm whose host cannot be dialled.
		"realm dial fails": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+other(closed)+`/token",service="s"`)
			w.WriteHeader(http.StatusUnauthorized)
		},
		"realm answers 403": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+other(srvPort)+`/token",service="s"`)
			w.WriteHeader(http.StatusUnauthorized)
		},
		"realm is not https": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+marker+`.example.com/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := wildcardServer(t, http.HandlerFunc(h))
			defer srv.Close()
			srvPort = port(srv)
			c := newTestClient(t, nil, testGuard(), nil, srv)
			registry := host(srv)
			ref := mustRef(t, registry+"/team/app:1.0")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			v, err := NewAdmitter(c, AdmitterOptions{}).Admit(ctx, Requirements{Ref: ref.Registry + "/team/app:1.0", AnyOf: []*Signer{{}}})
			var u *Unavailable
			if !errors.As(err, &u) || v.Reason != AdmissionUnavailable {
				t.Fatalf("verdict %+v, err %v: want image_admission_unavailable from an *Unavailable", v, err)
			}
			for what, got := range map[string]string{"Detail": v.Detail, "Error()": err.Error(), "log": LogRegistry(err)} {
				if strings.Contains(got, marker) || !strings.Contains(got, registry) {
					t.Errorf("%s = %q: want the registry %s, never the %s host", what, got, registry, marker)
				}
			}
		})
	}
}
