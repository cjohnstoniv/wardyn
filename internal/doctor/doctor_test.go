// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestReportFailsOnlyOnAFailedStage(t *testing.T) {
	if !(Report{Stages: []Stage{OKStage("a", ""), SkipStage("b", "not applicable")}}).OK() {
		t.Error("ok and skip must not fail a report")
	}
	if (Report{Stages: []Stage{OKStage("a", ""), FailStage("b", "x", "NAME", "y")}}).OK() {
		t.Error("a failed stage must fail the report")
	}
}

func TestFailStageNamesItsSettingInSetFormAndJSON(t *testing.T) {
	r := Report{Stages: []Stage{FailStage("tls", "no root signed it", "WARDYN_TRUSTED_CA_FILE", "a PEM file")}}
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	if want := "FAIL  tls          no root signed it; set WARDYN_TRUSTED_CA_FILE to a PEM file\n"; b.String() != want {
		t.Errorf("text = %q, want %q", b.String(), want)
	}
	b.Reset()
	if err := r.WriteJSON(&b); err != nil {
		t.Fatal(err)
	}
	var got struct {
		OK     bool
		Stages []Stage
	}
	if err := json.Unmarshal([]byte(b.String()), &got); err != nil || got.OK || got.Stages[0].Setting != "WARDYN_TRUSTED_CA_FILE" || got.Stages[0].Status != StatusFail {
		t.Errorf("json = %s (%v)", b.String(), err)
	}
}

// TestNoSecretReachesEitherFormat runs a failing and a passing route with a token and a proxy URL that holds a
// password, adds a stage whose text quotes both, and checks that neither format carries any of them.
func TestNoSecretReachesEitherFormat(t *testing.T) {
	const token, password = "tok-secret-1234", "p4ssw0rd-hunter2"
	o := newOrg(t, healthy)
	proxyURL := proxyThatAnswers(t, http.StatusProxyAuthRequired)
	proxyURL = strings.Replace(proxyURL, "http://", "http://svc-account:"+password+"@", 1)
	for name, n := range map[string]Network{
		"proxy refuses": {OrgURL: o.srv.URL, Source: "WARDYN_URL", OrgSetting: "WARDYN_URL", Identity: true, Token: token, TokenSource: "WARDYN_TOKEN", ProxyFor: viaProxy(t, proxyURL)},
		"healthy":       {OrgURL: o.srv.URL, Source: "WARDYN_URL", OrgSetting: "WARDYN_URL", Identity: true, Token: token, TokenSource: "WARDYN_TOKEN", SystemRoots: o.pool, ProxyFor: direct},
	} {
		t.Run(name, func(t *testing.T) {
			r := Report{Stages: n.Run(context.Background())}
			r.Stages = append(r.Stages, FailStage("engine", "ssh://deploy:"+password+"@engine.example.com refused", "DOCKER_HOST", "x"))
			r = r.Scrubbed()
			var text, js strings.Builder
			_ = r.WriteText(&text)
			_ = r.WriteJSON(&js)
			for _, out := range []string{text.String(), js.String()} {
				for _, secret := range []string{password, "svc-account", "deploy:", token} {
					if strings.Contains(out, secret) {
						t.Errorf("the output carries %q:\n%s", secret, out)
					}
				}
			}
		})
	}
}

func TestScrubRemovesURLCredentialsAndExactSecrets(t *testing.T) {
	got := Scrub("dial http://user:pw@proxy.example.com:3128 and wss://t@org.example.com with abcd1234", "abcd1234")
	if strings.Contains(got, "pw") || strings.Contains(got, "user:") || strings.Contains(got, "t@") || strings.Contains(got, "abcd1234") || !strings.Contains(got, "proxy.example.com:3128") {
		t.Errorf("Scrub = %q", got)
	}
}
