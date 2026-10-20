// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerConfigName is the file's name under <UserConfigDir>/wardyn.
const runnerConfigName = "runnerd.json"

// maxRunnerConfigBytes bounds the file: it holds a dozen short settings.
const maxRunnerConfigBytes = 64 << 10

// runnerFile is runnerd.json, the one place a runner's settings have a name and a default. It never holds
// a credential (checkRunnerFile refuses a proxy URL that carries one), so it is mode 0600 for tidiness,
// not secrecy.
type runnerFile struct {
	OrgURL   string `json:"org_url,omitempty"`
	StateDir string `json:"state_dir,omitempty"`
	// DockerHost is the engine socket; empty follows the docker CLI's context, as serve always has.
	DockerHost string `json:"docker_host,omitempty"`
	// ProxyImage pins the proxy image: serve never replaces it with the organisation's answer.
	ProxyImage string `json:"proxy_image,omitempty"`
	// ProxyImageMirror is a repository (no tag) that holds a copy of the proxy image; serve takes the tag
	// the organisation names and pulls it from here.
	ProxyImageMirror string `json:"proxy_image_mirror,omitempty"`
	RelayListen      string `json:"relay_listen,omitempty"`
	RelayHostTarget  string `json:"relay_host_target,omitempty"`
	InternalNetwork  string `json:"internal_network,omitempty"`
	TrustedCAFile    string `json:"trusted_ca_file,omitempty"`
	// OrgCAFile holds extra roots for the runner's own connection to the organisation, and for nothing else.
	OrgCAFile string `json:"org_ca_file,omitempty"`
	// UpstreamProxy is the proxy every run's egress chains through; RunnerProxy is the one runnerd's own
	// connection to the organisation uses (its environment twin is HTTPS_PROXY).
	UpstreamProxy string `json:"upstream_proxy,omitempty"`
	RunnerProxy   string `json:"runner_proxy,omitempty"`
	// RunnerNoProxy is the NO_PROXY list that goes with RunnerProxy.
	RunnerNoProxy  string `json:"runner_no_proxy,omitempty"`
	RecordingStore string `json:"recording_store,omitempty"`
}

// Where each setting is read from, in the order serve consults them. A managed preference outranks the
// environment, as it did before this file existed: a device administrator's profile is not something a
// user's environment variable can replace.
const (
	sourceFlag    = "flag"
	sourceManaged = "managed preference"
	sourceEnv     = "environment"
	sourceFile    = "file"
	sourceDefault = "default"
)

// runnerField names one setting everywhere it can come from. The zero flag, env or managed is "none".
type runnerField struct {
	name    string
	flag    string
	env     []string
	managed string
	def     string
	file    func(*runnerFile) *string
}

var runnerFields = []runnerField{
	{name: "org_url", flag: "org", managed: "OrgURL", file: func(f *runnerFile) *string { return &f.OrgURL }},
	{name: "state_dir", flag: "state-dir", file: func(f *runnerFile) *string { return &f.StateDir }},
	{name: "docker_host", env: []string{"DOCKER_HOST"}, managed: "DockerHost", file: func(f *runnerFile) *string { return &f.DockerHost }},
	{name: "proxy_image", flag: "proxy-image", env: []string{"WARDYN_PROXY_IMAGE"}, managed: "ProxyImage", file: func(f *runnerFile) *string { return &f.ProxyImage }},
	{name: "proxy_image_mirror", file: func(f *runnerFile) *string { return &f.ProxyImageMirror }},
	{name: "relay_listen", flag: "relay-listen", env: []string{"WARDYN_RELAY_LISTEN"}, managed: "RelayListen", file: func(f *runnerFile) *string { return &f.RelayListen }},
	{name: "relay_host_target", flag: "relay-host-target", env: []string{"WARDYN_RELAY_HOST_TARGET"}, managed: "RelayHostTarget", file: func(f *runnerFile) *string { return &f.RelayHostTarget }},
	{name: "internal_network", flag: "internal-network", env: []string{"WARDYN_INTERNAL_NETWORK"}, managed: "InternalNetwork", def: "wardyn-internal", file: func(f *runnerFile) *string { return &f.InternalNetwork }},
	{name: "trusted_ca_file", flag: "trusted-ca-file", env: []string{"WARDYN_TRUSTED_CA_FILE"}, managed: "TrustedCAFile", file: func(f *runnerFile) *string { return &f.TrustedCAFile }},
	{name: "org_ca_file", flag: "org-ca-file", env: []string{"WARDYN_ORG_CA_FILE"}, managed: "OrgCAFile", file: func(f *runnerFile) *string { return &f.OrgCAFile }},
	{name: "upstream_proxy", flag: "upstream-proxy", env: []string{"WARDYN_UPSTREAM_PROXY"}, file: func(f *runnerFile) *string { return &f.UpstreamProxy }},
	{name: "runner_proxy", env: []string{"HTTPS_PROXY", "https_proxy"}, file: func(f *runnerFile) *string { return &f.RunnerProxy }},
	{name: "runner_no_proxy", env: []string{"NO_PROXY", "no_proxy"}, file: func(f *runnerFile) *string { return &f.RunnerNoProxy }},
	{name: "recording_store", flag: "recording-store", env: []string{"WARDYN_RECORDING_STORE"}, managed: "RecordingStore", def: "pg", file: func(f *runnerFile) *string { return &f.RecordingStore }},
}

// resolvedField is one setting's effective value and where it came from, for serve and for any command
// that has to name the field to change.
type resolvedField struct {
	Name, Value string
	// Source reads "flag --org", "managed preference OrgURL", "environment DOCKER_HOST", "file <path>" or "default".
	Source string
}

type runnerConfig []resolvedField

// get is the effective value of a field, "" when it is unset.
func (c runnerConfig) get(name string) string { return c.field(name).Value }

func (c runnerConfig) field(name string) resolvedField {
	i := slices.IndexFunc(c, func(f resolvedField) bool { return f.Name == name })
	if i < 0 {
		return resolvedField{Name: name, Source: sourceDefault}
	}
	return c[i]
}

// runnerInputs is everything resolution reads, so a test runs it without a process environment.
type runnerInputs struct {
	// flag is the command-line value of a flag the person gave, if they gave it.
	flag     func(name string) (string, bool)
	getenv   func(string) string
	managed  func(key string) string
	file     runnerFile
	filePath string
}

// resolveRunnerConfig applies flag > managed preference > environment > file > default to every field.
func resolveRunnerConfig(in runnerInputs) runnerConfig {
	out := make(runnerConfig, 0, len(runnerFields))
	for _, f := range runnerFields {
		out = append(out, f.resolve(in))
	}
	return out
}

func (f runnerField) resolve(in runnerInputs) resolvedField {
	got := func(value, source string) resolvedField {
		return resolvedField{Name: f.name, Value: value, Source: source}
	}
	if f.flag != "" && in.flag != nil {
		if v, ok := in.flag(f.flag); ok && strings.TrimSpace(v) != "" {
			return got(strings.TrimSpace(v), sourceFlag+" --"+f.flag)
		}
	}
	if f.managed != "" && in.managed != nil {
		if v := in.managed(f.managed); v != "" {
			return got(v, sourceManaged+" "+f.managed)
		}
	}
	for _, name := range f.env {
		if v := strings.TrimSpace(in.getenv(name)); v != "" {
			return got(v, sourceEnv+" "+name)
		}
	}
	file := in.file
	if v := *f.file(&file); v != "" {
		return got(v, sourceFile+" "+in.filePath)
	}
	return got(f.def, sourceDefault)
}

// defaultRunnerConfigPath is <UserConfigDir>/wardyn/runnerd.json, the file serve reads when --config is not given.
func defaultRunnerConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "wardyn", runnerConfigName), nil
}

// loadRunnerFile reads and validates runnerd.json. A missing file is the empty one, unless the person named
// it (explicit): then it is an error. Every refusal names the file and the field.
func loadRunnerFile(path string, explicit bool) (runnerFile, error) {
	var f runnerFile
	fh, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) && !explicit {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("runner config %s: %w", path, err)
	}
	defer fh.Close()
	raw, err := io.ReadAll(io.LimitReader(fh, maxRunnerConfigBytes+1))
	if err != nil {
		return f, fmt.Errorf("runner config %s: %w", path, err)
	}
	if len(raw) > maxRunnerConfigBytes {
		return f, fmt.Errorf("runner config %s: larger than %d KiB", path, maxRunnerConfigBytes>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return runnerFile{}, fmt.Errorf("runner config %s: %w", path, err)
	}
	if dec.More() {
		return runnerFile{}, fmt.Errorf("runner config %s: more than one JSON value", path)
	}
	if err := checkRunnerFile(f); err != nil {
		return runnerFile{}, fmt.Errorf("runner config %s: %w", path, err)
	}
	return f, nil
}

// checkRunnerFile is the field-by-field check serve runs at startup and setup runs before it writes.
func checkRunnerFile(f runnerFile) error {
	if f.OrgURL != "" {
		if err := federation.CheckOrgURL(f.OrgURL); err != nil {
			return fmt.Errorf("org_url: %w", err)
		}
	}
	for _, p := range []struct{ name, v string }{{"state_dir", f.StateDir}, {"trusted_ca_file", f.TrustedCAFile}, {"org_ca_file", f.OrgCAFile}} {
		if p.v != "" && !filepath.IsAbs(p.v) {
			return fmt.Errorf("%s: must be an absolute path", p.name)
		}
	}
	for _, p := range []struct{ name, v string }{{"proxy_image", f.ProxyImage}, {"proxy_image_mirror", f.ProxyImageMirror}} {
		if p.v != "" && !types.SafeImageRef(p.v) {
			return fmt.Errorf("%s: not a usable image reference", p.name)
		}
	}
	if m := f.ProxyImageMirror; m != "" && strings.ContainsAny(path.Base(m), ":@") {
		return errors.New("proxy_image_mirror: a repository without a tag or digest, such as registry.example/wardyn-proxy; the tag follows the organisation")
	}
	if _, err := parseRelayWant(f.RelayListen, f.RelayHostTarget); err != nil {
		return fmt.Errorf("relay_listen / relay_host_target: %w", err)
	}
	if f.InternalNetwork != "" && !safeSetting.MatchString(f.InternalNetwork) {
		return errors.New("internal_network: unsupported characters")
	}
	if f.RecordingStore != "" && !slices.Contains([]string{"pg", "fs", "off"}, f.RecordingStore) {
		return errors.New("recording_store: must be pg, fs or off")
	}
	if err := refuseProxyCredential("upstream_proxy", f.UpstreamProxy, "WARDYN_UPSTREAM_PROXY"); err != nil {
		return err
	}
	if err := refuseProxyCredential("runner_proxy", f.RunnerProxy, "HTTPS_PROXY", "http", "https", "socks5", "socks5h"); err != nil {
		return err
	}
	if err := proxy.ValidUpstreamProxyURL(f.UpstreamProxy); err != nil {
		return fmt.Errorf("upstream_proxy: %w", err)
	}
	return nil
}

// refuseProxyCredential keeps a credential out of the file. The value is never repeated in the error. A value
// without a scheme is read the way Go's proxy-from-environment reads it, as http://<value>, so a scheme-less
// "user:pass@host:port" is refused as the credential it is; scheme, when the field limits it, is checked
// after that.
func refuseProxyCredential(field, raw, envName string, schemes ...string) error {
	if raw == "" {
		return nil
	}
	// Go's proxy-from-environment parses the value and, when that fails or leaves no scheme or host, reads it
	// again as http://<value>. Do the same, so a credential cannot hide as a "scheme".
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		u, err = url.Parse("http://" + raw)
	}
	if err != nil {
		return fmt.Errorf("%s: not a URL (the value is not repeated here: it may hold a credential)", field)
	}
	// The text is checked as well as the parse: a value the re-parse cannot split (a bad port, a space, an
	// unclosed bracket) still holds its credential, and setup refuses by the same rule.
	if _, had := stripUserinfo(raw); had || u.User != nil {
		return fmt.Errorf("%s: carries a user name or password, and this file never holds a credential: remove it here and set %s in the service's environment instead "+
			"(a systemd drop-in EnvironmentFile on Linux; see docs/runner-registration.md#a-runner-behind-a-corporate-proxy)", field, envName)
	}
	if len(schemes) > 0 && !slices.Contains(schemes, strings.ToLower(u.Scheme)) {
		return fmt.Errorf("%s: the scheme is not one of %s", field, strings.Join(schemes, ", "))
	}
	return nil
}

// mirrorProxyImage is image pulled from the mirror repository instead, with the tag or digest it already
// has; an image with neither is returned unchanged, since there is nothing for the mirror to follow.
func mirrorProxyImage(mirror, image string) string {
	if mirror == "" {
		return image
	}
	ref, digest, hasDigest := strings.Cut(image, "@")
	tag := ""
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		tag = ref[i+1:]
	}
	if tag == "" && !hasDigest {
		return image
	}
	out := mirror
	if tag != "" {
		out += ":" + tag
	}
	if hasDigest {
		out += "@" + digest
	}
	return out
}

// mirroredProxyImageHook has an unpinned runner pull the image the organisation names from the mirror.
func mirroredProxyImageHook(mirror string, pinned bool, follow func(string)) func(string) {
	if pinned {
		return follow
	}
	return func(answered string) { follow(mirrorProxyImage(mirror, answered)) }
}

// effectiveRunnerConfig is the whole resolution for a command that is not serve (a doctor naming the field to
// change): the file at path ("" is the default one), the process environment and the managed preferences.
// flag reports a flag the person gave; nil means none.
func effectiveRunnerConfig(path string, flag func(string) (string, bool)) (runnerConfig, error) {
	explicit := path != ""
	var err error
	if path, err = cmpOrDefaultPath(path); err != nil {
		return nil, err
	}
	file, err := loadRunnerFile(path, explicit)
	if err != nil {
		return nil, err
	}
	return resolveRunnerConfig(runnerInputs{flag: flag, getenv: os.Getenv, managed: managedPref, file: file, filePath: path}), nil
}

func cmpOrDefaultPath(p string) (string, error) {
	if p != "" {
		return p, nil
	}
	return defaultRunnerConfigPath()
}

// writeRunnerFile writes runnerd.json atomically at mode 0600 after checking it, so a refused value never
// reaches the disk.
func writeRunnerFile(path string, f runnerFile) error {
	if err := checkRunnerFile(f); err != nil {
		return fmt.Errorf("runner config %s: %w", path, err)
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, 0o600, func(w io.Writer) error { _, err := w.Write(append(raw, '\n')); return err })
}
