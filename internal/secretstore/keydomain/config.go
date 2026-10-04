// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package keydomain holds key domains: tenants of the deployment's key service,
// each a Transit key or a Key Vault key pair, that a subject's principal keys
// are wrapped under. Domains come only from the file WARDYN_KEY_DOMAINS_FILE
// names, and the database only says which domain a subject's next key
// generation goes to (Service). A database writer can therefore move where a
// subject's FUTURE keys are written, never declare a key of their own.
package keydomain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// Default is the deployment's credential KEK (WARDYN_KEK): the domain of every
// subject no assignment names. It is never declared in the file.
const Default = "default"

// MaxNameLen bounds a domain name.
const MaxNameLen = 63

var nameShape = regexp.MustCompile(`^[a-z0-9-]+$`)

// File is the parsed WARDYN_KEY_DOMAINS_FILE: domain name to where its key is.
type File map[string]Entry

// Entry is one domain: exactly one of Transit and AzureKV.
type Entry struct {
	Transit *Transit `json:"transit,omitempty"`
	AzureKV *AzureKV `json:"azurekv,omitempty"`
}

// Transit is a Vault Transit key in the install's Transit mount. Role, when
// set, is the Kubernetes-auth role the domain's key is reached as; empty means
// the credential role (WARDYN_VAULT_ROLE).
type Transit struct {
	Key  string `json:"key"`
	Role string `json:"role,omitempty"`
}

// AzureKV is a Key Vault key pair: the RSA key that wraps and the EC key that
// signs each wrap. ClientID, when set, is the Entra identity the pair is
// reached as; empty means WARDYN_AZURE_CLIENT_ID.
type AzureKV struct {
	Key        string `json:"key"`
	SigningKey string `json:"signingKey"`
	ClientID   string `json:"clientId,omitempty"`
}

// ValidName reports why name may not name a domain: it must be 1 to MaxNameLen
// characters of [a-z0-9-], and not Default, which the file never declares.
func ValidName(name string) error {
	switch {
	case name == "":
		return errors.New("a domain name is empty")
	case name == Default:
		return fmt.Errorf("a domain is named %q, which is the credential key's own domain and is never declared", Default)
	case len(name) > MaxNameLen || !nameShape.MatchString(name):
		return fmt.Errorf("domain name %q must be 1 to %d characters of a-z, 0-9 and \"-\"", name, MaxNameLen)
	}
	return nil
}

// Parse decodes and validates a domains file: a JSON object from domain name to
// {"transit": {"key", "role"}} or {"azurekv": {"key", "signingKey", "clientId"}}.
// An empty document or {} declares no domain. Unknown fields are refused, so a
// misspelled setting never silently drops a key.
func Parse(data []byte) (File, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return File{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("not a JSON object from domain name to key: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing content after the JSON object")
	}
	for _, name := range f.Names() {
		if err := ValidName(name); err != nil {
			return nil, err
		}
		if err := f[name].validate(name); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (e Entry) validate(name string) error {
	switch {
	case e.Transit != nil && e.AzureKV != nil:
		return fmt.Errorf("domain %q names both a transit key and a key vault pair; name one", name)
	case e.Transit != nil:
		if strings.TrimSpace(e.Transit.Key) == "" {
			return fmt.Errorf("domain %q: transit needs a key", name)
		}
	case e.AzureKV != nil:
		if strings.TrimSpace(e.AzureKV.Key) == "" || strings.TrimSpace(e.AzureKV.SigningKey) == "" {
			return fmt.Errorf("domain %q: azurekv needs key and signingKey, set together", name)
		}
	default:
		return fmt.Errorf("domain %q names no key: give it transit or azurekv", name)
	}
	return nil
}

// Load reads and parses the domains file at path; an empty path declares none.
func Load(path string) (File, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return File{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("WARDYN_KEY_DOMAINS_FILE: %w", err)
	}
	f, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("WARDYN_KEY_DOMAINS_FILE %s: %w", path, err)
	}
	return f, nil
}

// Names is the declared domain names, sorted.
func (f File) Names() []string {
	names := make([]string, 0, len(f))
	for n := range f {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// KeyDescriptions says, for each declared domain, where its key is, for display:
// "Transit key finance" or "Key Vault key <id>". The key's name is a configured
// identifier, never a secret.
func (f File) KeyDescriptions() map[string]string {
	out := make(map[string]string, len(f))
	for name, e := range f {
		switch {
		case e.Transit != nil:
			out[name] = "Transit key " + strings.TrimSpace(e.Transit.Key)
		case e.AzureKV != nil:
			out[name] = "Key Vault key " + strings.TrimSpace(e.AzureKV.Key)
		}
	}
	return out
}
