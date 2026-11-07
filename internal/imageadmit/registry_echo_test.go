// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A registry that was sent a Basic credential can put it in any field it serves. These markers stand for that
// credential; no refusal detail or error text may repeat them. The number goes where only a JSON number is
// read, since a JSON error quotes a number it could not decode.
const (
	echoMarker    = "leakmarker"
	echoMarkerNum = "31337424242"
)

func assertNoEcho(t *testing.T, what, got string) {
	t.Helper()
	if l := strings.ToLower(got); strings.Contains(l, echoMarker) || strings.Contains(l, echoMarkerNum) {
		t.Errorf("%s = %q repeats a value the registry served", what, got)
	}
}

// TestRefusalDetailNeverRepeatsSignatureMaterial: the legacy .sig/.att layers and the referrer bundles are
// registry content, so a refusal names what was wrong with them, never what they held.
func TestRefusalDetailNeverRepeatsSignatureMaterial(t *testing.T) {
	digest := sum256([]byte("image manifest"))
	sigLayer := func(blob string, ann map[string]string) LegacyLayer {
		return LegacyLayer{Descriptor: Descriptor{MediaType: LegacySignatureMediaType, Digest: sum256([]byte(blob)),
			Size: int64(len(blob)), Annotations: ann}, Blob: []byte(blob)}
	}
	goodPayload := `{"critical":{"type":"` + simpleSigningType + `","image":{"docker-manifest-digest":"` + digest + `"}}}`
	layers := map[string]LegacyLayer{
		"descriptor digest": {Descriptor: Descriptor{MediaType: LegacySignatureMediaType, Digest: "sha256:" + echoMarker, Size: 2}, Blob: []byte("{}")},
		"media type": func() LegacyLayer {
			l := sigLayer("{}", nil)
			l.Descriptor.MediaType = "application/" + echoMarker
			return l
		}(),
		"payload type":   sigLayer(`{"critical":{"type":"`+echoMarker+`"}}`, nil),
		"payload digest": sigLayer(`{"critical":{"type":"`+simpleSigningType+`","image":{"docker-manifest-digest":"sha256:`+echoMarker+`"}}}`, nil),
		"payload JSON":   sigLayer(`{"critical":{"type":`+echoMarkerNum+`}}`, nil),
		"rekor JSON":     sigLayer(goodPayload, map[string]string{annotationBundle: `{"Payload":{"body":` + echoMarkerNum + `}}`}),
		"rekor body":     sigLayer(goodPayload, map[string]string{annotationBundle: `{"Payload":{"body":"` + echoMarker + `"}}`}),
		"envelope JSON": func() LegacyLayer {
			l := sigLayer(`{"payloadType":`+echoMarkerNum+`}`, nil)
			l.Descriptor.MediaType = LegacyAttestationMediaType
			return l
		}(),
	}
	req := Requirements{AnyOf: []*Signer{{name: "corp"}}}
	for name, l := range layers {
		if _, err := legacyCandidate(l, digest); err == nil {
			t.Errorf("legacy %s: accepted", name)
		} else {
			assertNoEcho(t, "legacy "+name+" error", err.Error())
		}
		v := CheckCandidates(req, digest, Candidates{Legacy: []LegacyLayer{l}})
		if v.Reason != SignatureMissing || v.Detail == "" {
			t.Errorf("legacy %s: verdict %+v, want signature_missing with a detail", name, v)
		}
		assertNoEcho(t, "legacy "+name+" Detail", v.Detail)
	}
	bundles := map[string]string{
		"bundle media type":    `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json;` + echoMarker + `"}`,
		"bundle unknown field": `{"` + echoMarker + `":1}`,
	}
	for name, b := range bundles {
		v := CheckCandidates(req, digest, Candidates{Bundles: [][]byte{[]byte(b)}})
		if v.Reason != SignatureMissing || v.Detail == "" {
			t.Errorf("%s: verdict %+v, want signature_missing with a detail", name, v)
		}
		assertNoEcho(t, name+" Detail", v.Detail)
	}
}

// TestRegistryErrorsNeverRepeatWhatTheRegistryServed covers manifests, descriptors, redirects and challenges.
func TestRegistryErrorsNeverRepeatWhatTheRegistryServed(t *testing.T) {
	if _, err := parseManifest([]byte(`{}`), http.Header{}, "sha256:"+echoMarker); err == nil {
		t.Error("a manifest that is not the digest asked for was accepted")
	} else {
		assertNoEcho(t, "manifest digest mismatch", err.Error())
	}
	if _, err := parseManifest([]byte(`{"mediaType":`+echoMarkerNum+`}`), http.Header{}, ""); err == nil {
		t.Error("an unreadable manifest was accepted")
	} else {
		assertNoEcho(t, "manifest JSON", err.Error())
	}
	if _, err := newTestClient(t, nil, testGuard(), nil).Blob(context.Background(), mustRef(t, "example.com/team/app"),
		Descriptor{Digest: "sha256:" + echoMarker, Size: 1}); err == nil {
		t.Error("a malformed blob digest was accepted")
	} else {
		assertNoEcho(t, "blob digest", err.Error())
	}

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	_, closed, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	var srvPort string
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"redirect scheme": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, echoMarker+"://example.com/x", http.StatusFound)
		},
		"redirect path": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.com:"+closed+"/"+echoMarker, http.StatusFound)
		},
		"realm scheme": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+echoMarker+`://example.com/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		},
		"realm path and service": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://example.com:`+closed+`/`+echoMarker+`",service="`+echoMarker+`"`)
			w.WriteHeader(http.StatusUnauthorized)
		},
		"realm redirects away": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				http.Redirect(w, r, "https://127.0.0.1:"+srvPort+"/"+echoMarker, http.StatusTemporaryRedirect)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://example.com:`+srvPort+`/token",scope="`+echoMarker+`"`)
			w.WriteHeader(http.StatusUnauthorized)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(h))
			defer srv.Close()
			srvPort = port(srv)
			c := newTestClient(t, nil, testGuard(), nil, srv)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			v, err := NewAdmitter(c, AdmitterOptions{}).Admit(ctx, Requirements{Ref: host(srv) + "/team/app:1.0", AnyOf: []*Signer{{}}})
			if err == nil || v.Reason != AdmissionUnavailable {
				t.Fatalf("verdict %+v, err %v: want image_admission_unavailable", v, err)
			}
			assertNoEcho(t, "Detail", v.Detail)
			assertNoEcho(t, "Error()", err.Error())
		})
	}
}
