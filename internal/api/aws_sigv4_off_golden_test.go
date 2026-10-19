// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The two AWS sign-in lanes dispatch exactly what they did before proxy-side SigV4 re-signing
// existed, while its switch is off: the proxy config and the sandbox env, byte for byte up to
// the values every dispatch mints afresh (ids, the per-run CA, the run token, timestamps).
//
// Regenerate with: WARDYN_UPDATE_GOLDEN=1 go test ./internal/api/ -run TestAWSSigV4Off
func TestAWSSigV4Off_DispatchIsUnchanged(t *testing.T) {
	for name, dispatch := range map[string]func(t *testing.T, on func(*Config)) runner.SandboxSpec{
		"bedrock_sso": dispatchBedrockSSORun,
		"aws_cli":     dispatchAWSCLIRun,
	} {
		t.Run(name, func(t *testing.T) {
			spec := dispatch(t, func(*Config) {})
			raw, err := runner.BuildProxyConfig(spec.RunID, spec.ProxyConfig, runner.ProxyListenPort)
			if err != nil {
				t.Fatal(err)
			}
			var pc any
			if err := json.Unmarshal(raw, &pc); err != nil {
				t.Fatal(err)
			}
			env := map[string]any{}
			for k, v := range spec.Env {
				env[k] = goldenValue(k, v)
			}
			secretKeys := make([]string, 0, len(spec.SecretEnv))
			for k := range spec.SecretEnv {
				secretKeys = append(secretKeys, k)
			}
			slices.Sort(secretKeys)
			got, err := json.MarshalIndent(normalizeGolden(map[string]any{
				"proxy_config": pc, "env": env, "secret_env_keys": secretKeys,
			}), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			path := "testdata/aws_sigv4_off_" + name + ".json"
			if os.Getenv("WARDYN_UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(want) != string(got)+"\n" {
				t.Fatalf("the %s dispatch changed with SigV4 re-signing off; it must stay what it was.\n--- %s ---\n%s\n--- this tree ---\n%s", name, path, want, got)
			}
		})
	}
}

// dispatchBedrockSSORun launches a bedrock_sso run on the run owner's live session, after set
// adjusts the server's config.
func dispatchBedrockSSORun(t *testing.T, set func(*Config)) runner.SandboxSpec {
	t.Helper()
	srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(awsSSOTestProvider())}, &capStore{}, nil)
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	srv.cfg.AWSSSOProxyInject = true
	set(&srv.cfg)
	blob := createRenewalBlob()
	blob.ExpiresAt = time.Now().Add(2 * time.Hour)
	storeSSOBlobFor(t, srv, createRenewalOwner, blob)
	if res := postCreate(t, srv); res.code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", res.code, res.body)
	}
	return lastDispatched(t, srv.cfg.Runner.(*fakeRunner))
}

// dispatchAWSCLIRun launches a run carrying the AWS CLI row on its owner's session, after set
// adjusts the server's config.
func dispatchAWSCLIRun(t *testing.T, set func(*Config)) runner.SandboxSpec {
	t.Helper()
	f, ref := cliAWSFixture(t)
	set(&f.srv.cfg)
	putCLIBlob(t, f.srv.cfg.Secrets.(*memSecrets), capSub, cliAWSBlob())
	if w := f.ask(t, componentDoors[0], componentBody(ref)); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	return lastDispatched(t, f.srv.cfg.Runner.(*fakeRunner))
}

func lastDispatched(t *testing.T, fr *fakeRunner) runner.SandboxSpec {
	t.Helper()
	fr.waitForSandbox(t)
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return fr.lastSpec
}

// goldenValue opens the staged ~/.aws records, whose base64 would hide a changed timestamp-free
// byte behind a changed timestamp.
func goldenValue(key, v string) any {
	if key != awsSSOConfigEnvVar {
		return v
	}
	files := map[string]string{}
	for rec := range strings.SplitSeq(v, "\n") {
		path, b64, _ := strings.Cut(rec, "\t")
		raw, _ := base64.StdEncoding.DecodeString(b64)
		files[path] = string(raw)
	}
	return files
}

var goldenVolatile = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]+-----.*?-----END [A-Z ]+-----\n?`), "<pem>"},
	{regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`), "<uuid>"},
	{regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)`), "<time>"},
	{regexp.MustCompile(`eyJ[\w-]*\.[\w-]*\.[\w-]*`), "<jwt>"},
}

// normalizeGolden replaces what each dispatch mints afresh in every string of v.
func normalizeGolden(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = normalizeGolden(e)
		}
	case map[string]string:
		out := map[string]any{}
		for k, e := range x {
			out[k] = normalizeGolden(e)
		}
		return out
	case []any:
		for i, e := range x {
			x[i] = normalizeGolden(e)
		}
	case string:
		for _, r := range goldenVolatile {
			x = r.re.ReplaceAllString(x, r.with)
		}
		return x
	}
	return v
}
