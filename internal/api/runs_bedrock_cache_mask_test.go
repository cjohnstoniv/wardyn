// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// What the AWS SSO cache file carries into a sandbox is the run's own to mask,
// beside the credential's shared registration: the shared row is let go when
// the credential moves on, and the run still holds the file.

const (
	cacheAccessToken  = "sso-access-token-in-the-cache-file"
	cacheClientSecret = "sso-client-secret-in-the-cache-file"
	cacheRefreshToken = "sso-refresh-token-never-in-the-file"
)

// ssoCacheDispatch builds a Bedrock SSO provider's transport for a new run as
// dispatch does, from a live blob, and returns the run, the staged cache file
// and the registry. The shared registration is made first, inside the call.
func ssoCacheDispatch(t *testing.T, proxyInject bool, refreshToken string) (uuid.UUID, string, *secretmask.Registry) {
	t.Helper()
	s := &Server{cfg: Config{
		MaskRegistry:      secretmask.NewRegistry(),
		Now:               func() time.Time { return awsSSOTestFixedNow },
		Audit:             &memAudit{},
		AWSSSOProxyInject: proxyInject,
	}}
	blob := awsSSOBlob{
		AccessToken: cacheAccessToken, RefreshToken: refreshToken, ClientID: "sso-client-id", ClientSecret: cacheClientSecret,
		StartURL: "https://example.awsapps.com/start", Region: "us-east-1", AccountID: "123456789012", RoleName: "WardynBedrockRole",
		ExpiresAt: awsSSOTestFixedNow.Add(time.Hour),
	}
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: awsSSOTestOwner}
	env := map[string]string{}
	tr := s.providerBedrockTransport(context.Background(), run, &types.RunPolicySpec{}, env,
		chosenProvider{provider: awsSSOTestProvider(), owner: awsSSOTestOwner}, blob)
	if err := tr.bedrock.maskErr; err != nil {
		t.Fatalf("the transport carries a masking error: %v", err)
	}
	var cache string
	for path, content := range decodeSSOFiles(t, env[awsSSOConfigEnvVar]) {
		if strings.Contains(path, "sso/cache/") {
			cache = content
		}
	}
	return run.ID, cache, s.cfg.MaskRegistry
}

// letGoOfSharedSSO retires the credential's shared registration and sweeps past
// its grace, as a rotation or a deletion followed by the hourly sweep does.
func letGoOfSharedSSO(t *testing.T, reg *secretmask.Registry) {
	t.Helper()
	sso := awsSSOTestScope()
	if err := reg.EvictGlobal(sso.rowOwner(), sso.ssoSecret(), awsSSOTestFixedNow); err != nil {
		t.Fatal(err)
	}
	reg.SweepGlobals(awsSSOTestFixedNow.Add(2 * time.Hour))
	if runMasks(reg, uuid.New(), cacheAccessToken) || runMasks(reg, uuid.New(), cacheClientSecret) {
		t.Fatal("the shared registration is still masked after it was retired and swept")
	}
}

func runMasks(reg *secretmask.Registry, run uuid.UUID, bare string) bool {
	return !bytes.Contains(reg.Masker(run).Mask([]byte("out "+bare+" end")), []byte(bare)) &&
		!bytes.Contains(reg.JSONVariantMasker(run).Mask([]byte(`{"v":"`+bare+`"}`)), []byte(bare))
}

// With the proxy lane off and no refresh token the file carries the access
// token and the client secret. Both are the run's own from dispatch, and both
// stay masked for the run after the shared registration is retired and swept.
func TestBedrockSSO_TheCacheFilesSecretsAreTheRunsOwn(t *testing.T) {
	run, cache, reg := ssoCacheDispatch(t, false, "")
	for _, bare := range []string{cacheAccessToken, cacheClientSecret} {
		if !strings.Contains(cache, bare) {
			t.Fatalf("the cache file does not carry %q: %s", bare, cache)
		}
	}
	if !slices.Contains(reg.RunIDs(), run) {
		t.Error("the run has no masking values of its own after dispatch")
	}
	letGoOfSharedSSO(t, reg)
	for _, bare := range []string{cacheAccessToken, cacheClientSecret} {
		if !runMasks(reg, run, bare) {
			t.Errorf("the run no longer masks %q, which its cache file still holds", bare)
		}
	}
}

// With a refresh token the registration fields are withheld from the file, so
// the access token alone is the run's own: nothing the file does not carry.
func TestBedrockSSO_OnlyWhatTheCacheFileCarriesIsTheRunsOwn(t *testing.T) {
	run, cache, reg := ssoCacheDispatch(t, false, cacheRefreshToken)
	if !strings.Contains(cache, cacheAccessToken) || strings.Contains(cache, cacheClientSecret) || strings.Contains(cache, cacheRefreshToken) {
		t.Fatalf("the cache file should carry the access token alone: %s", cache)
	}
	letGoOfSharedSSO(t, reg)
	if !runMasks(reg, run, cacheAccessToken) {
		t.Error("the run no longer masks the access token its cache file still holds")
	}
	if runMasks(reg, run, cacheClientSecret) || runMasks(reg, run, cacheRefreshToken) {
		t.Error("the run masks a value its cache file never carried")
	}
}

// With the proxy lane on the file carries a placeholder: nothing real is
// staged, and nothing is added for the run.
func TestBedrockSSO_APlaceholderCacheFileAddsNothingForTheRun(t *testing.T) {
	run, cache, reg := ssoCacheDispatch(t, true, "")
	if strings.Contains(cache, cacheAccessToken) || strings.Contains(cache, cacheClientSecret) {
		t.Fatalf("a proxy-injected cache file carries a real secret: %s", cache)
	}
	if slices.Contains(reg.RunIDs(), run) {
		t.Error("a run whose cache file is a placeholder was given masking values of its own")
	}
	letGoOfSharedSSO(t, reg)
	if runMasks(reg, run, cacheAccessToken) || runMasks(reg, run, cacheClientSecret) {
		t.Error("the run masks a value that never reached its sandbox")
	}
}

// The values are recorded before anything is staged. A run whose cache file
// cannot be put on record is refused by the dispatch phase itself, failed, and
// its sandbox env carries no SSO file at all.
func TestDispatchWiring_AnUnrecordedCacheFileRefusesTheRunAndStagesNothing(t *testing.T) {
	s := ssoInjectServer(t, false)
	s.cfg.MaskRegistry.SetBackend(&gapCovMaskBackend{putRunErr: errors.New("the run's values could not be committed")})
	st := &mpRefuseStore{integStore: &integStore{govEscapeStore: newGovEscapeStore(&capStore{})}}
	s.cfg.Store = st
	ctx := context.Background()
	run, err := st.CreateRun(ctx, types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: wiringOwner,
		ModelProviderID: reauthProviderID, State: types.RunStarting})
	if err != nil {
		t.Fatal(err)
	}
	sandboxEnv := map[string]string{}
	if _, ok := s.resolveLLMInjections(ctx, run, dispatchParams{}, &types.RunPolicySpec{}, sandboxEnv, nil, "",
		artifactRedirectPlan{}, false, reauthProviderSite(reauthProvider()), true, false, bedrockCredUngraded()); ok {
		t.Error("the dispatch phase went on with a cache file whose secrets are not on record for the run")
	}
	if got, _ := st.GetRun(ctx, run.ID); got.State != types.RunFailed || st.hint != fmt.Sprintf(mpBRMaskFailed, reauthProviderID) {
		t.Errorf("run = %s %q, want FAILED with the masking refusal", got.State, st.hint)
	}
	if staged, found := sandboxEnv[awsSSOConfigEnvVar]; found {
		t.Errorf("an SSO file was staged for a run that was refused: %q", staged)
	}
}

// The same through the masking manifests every deployment keeps, against a real
// Postgres: the file's secrets are committed to the run's manifest at dispatch,
// every replica that loads it masks them for the run after the shared
// registration is retired and swept, and a run whose values cannot be put on
// record carries the error dispatch refuses on.
func TestMaskRegistry_ABedrockSSOCacheFilesSecretsAreInTheRunsManifest(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	ctx := t.Context()
	c := chosenProvider{provider: awsSSOTestProvider(), owner: maskOwner}
	blob := awsSSOBlob{
		AccessToken: cacheAccessToken, ClientID: "sso-client-id", ClientSecret: cacheClientSecret,
		StartURL: "https://example.awsapps.com/start", Region: "us-east-1", AccountID: "123456789012", RoleName: "WardynBedrockRole",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	staged := func(run types.AgentRun) error {
		env := map[string]string{}
		err := a.srv.providerBedrockTransport(ctx, run, &types.RunPolicySpec{}, env, c, blob).bedrock.maskErr
		if _, found := env[awsSSOConfigEnvVar]; found == (err != nil) {
			t.Errorf("an SSO file staged = %v with the masking error %v: a file is staged exactly when its secrets are on record", found, err)
		}
		return err
	}

	run := l.run()
	if !a.srv.beginMaskManifest(ctx, run) {
		t.Fatal("the manifest could not begin")
	}
	if err := staged(run); err != nil {
		t.Fatalf("the transport carries a masking error: %v", err)
	}
	if !a.srv.completeMaskManifest(ctx, run) {
		t.Fatal("the manifest could not complete")
	}
	if n := l.count(`SELECT count(*) FROM run_mask_values WHERE run_id=$1`, run.ID); n != 2 {
		t.Fatalf("the run's manifest holds %d values, want the access token and the client secret", n)
	}

	sso := c.awsScope()
	if err := a.reg.EvictGlobal(sso.rowOwner(), sso.ssoSecret(), time.Now()); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(2 * time.Hour)
	if _, err := a.reg.SweepPersisted(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	for name, rp := range map[string]replica{"the replica that dispatched": a, "the other replica": b} {
		rp.reg.SweepGlobals(cutoff)
		if !rp.srv.maskCovered(ctx, run.ID) {
			t.Fatalf("%s does not cover the run", name)
		}
		if runMasks(rp.reg, uuid.New(), cacheClientSecret) {
			t.Fatalf("%s still masks the shared registration after it was retired and swept", name)
		}
		for _, bare := range []string{cacheAccessToken, cacheClientSecret} {
			if !runMasks(rp.reg, run.ID, bare) {
				t.Errorf("%s no longer masks %q for the run, whose cache file still holds it", name, bare)
			}
		}
	}

	if err := staged(types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: maskOwner}); err == nil {
		t.Error("a run with no manifest to record its cache file in carries no error for dispatch to refuse on")
	}
}
