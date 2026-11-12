// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrCredentialHelper is returned for an auth file that names credsStore or credHelpers. wardynd never runs a
// credential helper: a helper is a program on the host, and the file is the whole credential interface.
var ErrCredentialHelper = errors.New("the registry auth file names credsStore or credHelpers, which wardynd never runs; put the credentials in the file itself")

// maxAuthFileBytes bounds the file read; a Docker config with a few hundred registries is far below it.
const maxAuthFileBytes = 1 << 20

// RegistryAuth is one registry's credential from the auth file.
type RegistryAuth struct {
	Username, Password string
	// IdentityToken is a refresh token exchanged at the token realm (Docker's identitytoken).
	IdentityToken string
}

// CredentialSource answers which credential, if any, is configured for exactly one registry host
// (host or host:port; the default port 443 is dropped). It never answers for a parent or a subdomain.
type CredentialSource interface {
	Lookup(host string) (RegistryAuth, bool, error)
}

// AuthFile reads the Docker config.json-shaped file named by WARDYN_REGISTRY_AUTH_FILE.
// An empty path means anonymous pulls. The file is re-read when its mtime or size changes, so a rotated
// Kubernetes Secret mount takes effect without a restart. Errors never carry file content.
type AuthFile struct {
	path string

	mu    sync.Mutex
	mtime time.Time
	size  int64
	have  bool
	auths map[string]RegistryAuth
	err   error
}

// NewAuthFile returns a reader for path ("" = anonymous).
func NewAuthFile(path string) *AuthFile { return &AuthFile{path: path} }

// Validate reads the file now, for the boot check: an unreadable or malformed file, or one naming a
// credential helper, refuses to start rather than failing every later admission.
func (a *AuthFile) Validate() error {
	if a.path == "" {
		return nil
	}
	_, _, err := a.Lookup("")
	return err
}

// Lookup returns the credential configured for host.
func (a *AuthFile) Lookup(host string) (RegistryAuth, bool, error) {
	if a.path == "" {
		return RegistryAuth{}, false, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	fi, err := os.Stat(a.path)
	if err != nil {
		return RegistryAuth{}, false, fmt.Errorf("registry auth file: %w", errors.Unwrap(err))
	}
	if !a.have || !fi.ModTime().Equal(a.mtime) || fi.Size() != a.size {
		a.auths, a.err = readAuthFile(a.path)
		a.mtime, a.size, a.have = fi.ModTime(), fi.Size(), true
	}
	if a.err != nil {
		return RegistryAuth{}, false, a.err
	}
	got, ok := a.auths[normalizeHost(host)]
	return got, ok, nil
}

func readAuthFile(path string) (map[string]RegistryAuth, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("registry auth file: %w", errors.Unwrap(err))
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxAuthFileBytes+1))
	if err != nil || len(raw) > maxAuthFileBytes {
		return nil, errors.New("registry auth file: unreadable or larger than 1 MiB")
	}
	var cfg struct {
		CredsStore  string            `json:"credsStore"`
		CredHelpers map[string]string `json:"credHelpers"`
		Auths       map[string]struct {
			Auth          string `json:"auth"`
			Username      string `json:"username"`
			Password      string `json:"password"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	// The decoder error is replaced: encoding/json can quote input bytes, and these bytes are credentials.
	if json.Unmarshal(raw, &cfg) != nil {
		return nil, errors.New("registry auth file: not a Docker config.json (invalid JSON)")
	}
	if cfg.CredsStore != "" || len(cfg.CredHelpers) > 0 {
		return nil, ErrCredentialHelper
	}
	out := make(map[string]RegistryAuth, len(cfg.Auths))
	for k, e := range cfg.Auths {
		ra := RegistryAuth{Username: e.Username, Password: e.Password, IdentityToken: e.IdentityToken}
		if e.Auth != "" {
			dec, err := base64.StdEncoding.DecodeString(e.Auth)
			user, pass, ok := strings.Cut(string(dec), ":")
			if err != nil || !ok {
				return nil, fmt.Errorf("registry auth file: the auth value for %q is not base64 user:password", k)
			}
			ra.Username, ra.Password = user, pass
		}
		if ra == (RegistryAuth{}) {
			continue
		}
		hk := normalizeHost(k)
		out[hk] = ra
		if hk == "index.docker.io" || hk == "docker.io" {
			out["registry-1.docker.io"] = ra // the host Docker Hub registry traffic actually goes to
		}
	}
	return out, nil
}

// normalizeHost reduces "https://Host:443/v1/" and "host" to the same key. Only the default https port is dropped.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if _, rest, ok := strings.Cut(h, "://"); ok {
		h = rest
	}
	h, _, _ = strings.Cut(h, "/")
	return strings.TrimSuffix(h, ":443")
}
