// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// ComponentKind names a thing a run can be given access to. Every kind but
// ComponentCustom is a closed, built-in kind with its own code; a component
// enforces nothing itself — it describes primitives policy and the proxy
// already enforce.
type ComponentKind string

const (
	ComponentAgent         ComponentKind = "agent"
	ComponentModelProvider ComponentKind = "model_provider"
	ComponentGitProvider   ComponentKind = "git_provider" // github | azure_devops
	ComponentGitPAT        ComponentKind = "git_pat"      // other forges
	ComponentSSHKey        ComponentKind = "ssh_key"
	ComponentWorkspace     ComponentKind = "workspace"
	ComponentDrive         ComponentKind = "drive"
	// ComponentCustom is the one open kind, and the only one that is stored.
	ComponentCustom ComponentKind = "custom"
)

// How one component secret reaches the run.
const (
	// ComponentDeliveryHeader: the proxy presents the secret in a request
	// header; the sandbox never holds it.
	ComponentDeliveryHeader = "header"
	// ComponentDeliveryEnv and ComponentDeliveryFile put the secret inside
	// the sandbox for the whole run.
	ComponentDeliveryEnv  = "env"
	ComponentDeliveryFile = "file"
)

// ComponentFileDelivery says whether this build can deliver a secret as a
// file. While false, Validate refuses the mode, so no definition can promise a
// delivery nothing performs. True since the file lane exists (the file_secret
// grant and its dispatch).
const ComponentFileDelivery = true

// Bounds on one definition and on one run's attachments.
const (
	MaxComponentHosts            = 32
	MaxComponentSecrets          = 8
	MaxComponentConfigKeys       = 32
	MaxComponentConfigValueBytes = 4096
	MaxComponentNameRunes        = 64
	MaxComponentRefs             = 8
	// MaxComponentEnvNameBytes bounds an env var or config key name;
	// MaxComponentHeaderFormatBytes the header value template, which the
	// proxy writes onto every injected request.
	MaxComponentEnvNameBytes      = 128
	MaxComponentHeaderFormatBytes = 512
)

// ComponentDefinition is the whole contract of a custom component: the hosts
// it reaches, the secrets it carries with how each is delivered, and plain
// (non-secret) environment. It holds secret names, never values.
type ComponentDefinition struct {
	Hosts   []string          `json:"hosts"`
	Secrets []ComponentSecret `json:"secrets,omitempty"`
	Config  map[string]string `json:"config,omitempty"`
}

// ComponentSecret is one secret a component carries.
type ComponentSecret struct {
	SecretName string `json:"secret_name"`
	// Shared marks an org component's secret as the operator's value rather
	// than the launching person's own. Header delivery only (Validate); that
	// it appears on an org row only is the caller's check, since a definition
	// does not know its owner.
	Shared   bool              `json:"shared,omitempty"`
	Delivery ComponentDelivery `json:"delivery"`
}

// ComponentDelivery says how one secret reaches the run; the mode decides
// which other fields apply.
type ComponentDelivery struct {
	Mode string `json:"mode"` // header | env | file
	// Host, Header and Format apply to header: Host is a bare host — no
	// port, never a wildcard — that one of the definition's hosts names bare
	// or with :443; an empty Header or Format takes the caller's default.
	Host   string `json:"host,omitempty"`
	Header string `json:"header,omitempty"`
	Format string `json:"format,omitempty"`
	// PlainHTTP lets a header credential travel without TLS. That it appears
	// on an org row only is the caller's check.
	PlainHTTP bool `json:"plain_http,omitempty"`
	// Var applies to env: the environment variable name.
	Var string `json:"var,omitempty"`
	// File applies to file: a single name, never a path. The directory is
	// fixed by the runner.
	File string `json:"file,omitempty"`
}

// Component is one stored custom component: an org row (Owner "") an admin
// wrote and grants, or a person's saved row (Owner is their principal).
type Component struct {
	ID         uuid.UUID           `json:"id"`
	Owner      string              `json:"owner,omitempty"`
	Name       string              `json:"name"`
	Definition ComponentDefinition `json:"definition"`
	// Version starts at 1 and moves by one on every update.
	Version   int       `json:"version"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ComponentRef attaches one component to a run: a stored one by ID, or a
// run-only definition given inline (never stored as a component). Name labels
// an inline one.
type ComponentRef struct {
	ID     *uuid.UUID           `json:"id,omitempty"`
	Inline *ComponentDefinition `json:"inline,omitempty"`
	Name   string               `json:"name,omitempty"`
}

// RunComponent is the snapshot of one component as a run was launched with
// it. It is a copy, not a reference: later edits to, or deletion of, the
// stored component do not change it.
//
// It is also the run's authorization record. Erasing the person who defined
// it clears every descriptive field and sets Erased; RunID, Ordinal and
// SelfDefined survive, so a revive still knows which doors the run needed.
type RunComponent struct {
	RunID uuid.UUID `json:"run_id"`
	// Ordinal is the component's position in the request.
	Ordinal int `json:"ordinal"`
	// SelfDefined is true for an inline component or one the launcher owns;
	// such a run needed the custom-component feature. False is an org
	// component, whose ComponentID names its own grant.
	SelfDefined bool `json:"self_defined"`
	// Erased is true once the person who defined it was erased: every field
	// below is then empty.
	Erased bool `json:"erased,omitempty"`
	// ComponentID is nil for an inline component.
	ComponentID *uuid.UUID `json:"component_id,omitempty"`
	// Owner is "" for an org component, else the person who defined it (the
	// launcher).
	Owner string `json:"owner,omitempty"`
	Name  string `json:"name"`
	// Version is the stored component's version at launch; 0 for inline.
	Version    int                 `json:"version"`
	Definition ComponentDefinition `json:"definition"`
}

var (
	componentEnvNameRE   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	componentFileTokenRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
)

// Validate checks the shape of a definition. It is the one rule every door
// that accepts a definition runs; every error names the field.
//
// validHost is the egress allowlist's entry rule (proxy.ValidDomainEntry). It
// is a parameter because that package imports this one.
//
// personDefined is true for a definition a person wrote — an inline one, or
// a stored row with an owner — and false for an org row (owner ""). A
// person's hosts must be DNS names, never IP addresses, and a person may not
// use Shared or PlainHTTP; org rows keep the admin's allowlist semantics.
//
// Left to the caller, because they depend on the deployment or the store:
// whether the named secrets exist and whose they are, reserved secret names,
// and every bound on the hosts themselves (deny lists, model hosts, hosts that
// already carry a credential — compare with Destination).
func (d ComponentDefinition) Validate(validHost func(string) error, personDefined bool) error {
	if err := validateComponentHosts(d.Hosts, validHost, personDefined); err != nil {
		return err
	}
	if err := validateComponentConfig(d.Config); err != nil {
		return err
	}
	if len(d.Secrets) > MaxComponentSecrets {
		return fmt.Errorf("secrets: %d entries exceeds the %d-secret limit", len(d.Secrets), MaxComponentSecrets)
	}
	// One credential per header host is the proxy's law, and two deliveries
	// into one variable or one file would silently overwrite each other.
	seen := map[string]int{}
	for i, s := range d.Secrets {
		if !SecretNameRE.MatchString(s.SecretName) {
			return fmt.Errorf("secrets[%d].secret_name: %q is not a valid secret name", i, s.SecretName)
		}
		target, err := validateComponentDelivery(s, d)
		if err != nil {
			return fmt.Errorf("secrets[%d].%w", i, err)
		}
		if personDefined && s.Shared {
			return fmt.Errorf("secrets[%d].shared: only an organisation component can use a shared secret", i)
		}
		if personDefined && s.Delivery.PlainHTTP {
			return fmt.Errorf("secrets[%d].delivery.plain_http: a header secret you define is only ever sent over TLS", i)
		}
		if first, dup := seen[target]; dup {
			return fmt.Errorf("secrets[%d].delivery: delivers to the same place as secrets[%d]", i, first)
		}
		seen[target] = i
	}
	return nil
}

func validateComponentHosts(hosts []string, validHost func(string) error, personDefined bool) error {
	if len(hosts) > MaxComponentHosts {
		return fmt.Errorf("hosts: %d entries exceeds the %d-host limit", len(hosts), MaxComponentHosts)
	}
	for i, h := range hosts {
		if err := validHost(h); err != nil {
			return fmt.Errorf("hosts[%d]: %w", i, err)
		}
		dest, err := ParseDestination(h)
		if err != nil {
			return fmt.Errorf("hosts[%d]: %w", i, err)
		}
		if personDefined {
			if err := refuseAddressLiteral(dest); err != nil {
				return fmt.Errorf("hosts[%d]: %q %w", i, h, err)
			}
		}
		// One spelling per host, so a header delivery's host matches its entry
		// here exactly and the proxy binds the credential to the host allowed.
		if canon := dest.String(); h != canon {
			return fmt.Errorf("hosts[%d]: write %q as %q", i, h, canon)
		}
		if slices.Contains(hosts[:i], h) {
			return fmt.Errorf("hosts[%d]: %q is listed twice", i, h)
		}
	}
	return nil
}

func validateComponentConfig(config map[string]string) error {
	if len(config) > MaxComponentConfigKeys {
		return fmt.Errorf("config: %d keys exceeds the %d-key limit", len(config), MaxComponentConfigKeys)
	}
	for k, v := range config {
		if err := validComponentEnvName(k); err != nil {
			return fmt.Errorf("config: key %w", err)
		}
		if len(v) > MaxComponentConfigValueBytes {
			return fmt.Errorf("config[%s]: value exceeds %d bytes", k, MaxComponentConfigValueBytes)
		}
		if !printable(v) {
			return fmt.Errorf("config[%s]: value must be printable text on one line", k)
		}
	}
	return nil
}

// validateComponentDelivery checks one secret's delivery and returns the
// place it delivers to, for the caller's duplicate check. Fields of another
// mode are refused rather than ignored: a row that looks like it names a
// variable it does not use is the ambiguity a closed shape exists to prevent.
func validateComponentDelivery(s ComponentSecret, d ComponentDefinition) (string, error) {
	del := s.Delivery
	if s.Shared && del.Mode != ComponentDeliveryHeader {
		return "", errors.New("shared: a shared secret can only be delivered as a header")
	}
	switch del.Mode {
	case ComponentDeliveryHeader:
		if del.Var != "" || del.File != "" {
			return "", errors.New("delivery: var and file are not part of header delivery")
		}
		if err := validHeaderHost(del.Host, d.Hosts); err != nil {
			return "", fmt.Errorf("delivery.host: %w", err)
		}
		if del.Header != "" && !egress.ValidHeaderName(del.Header) {
			return "", fmt.Errorf("delivery.header: %q is not a valid HTTP header name", del.Header)
		}
		if err := ValidInjectionFormat(del.Format); err != nil {
			return "", fmt.Errorf("delivery.%w", err)
		}
		// Go's transport refuses a control byte only when it writes the
		// request, which would fail the run late rather than here.
		if len(del.Format) > MaxComponentHeaderFormatBytes || !printable(del.Format) {
			return "", fmt.Errorf("delivery.format: must be printable text of at most %d bytes", MaxComponentHeaderFormatBytes)
		}
		return "header " + del.Host, nil
	case ComponentDeliveryEnv:
		if del.Host != "" || del.Header != "" || del.Format != "" || del.PlainHTTP || del.File != "" {
			return "", errors.New("delivery: only var is part of env delivery")
		}
		if err := validComponentEnvName(del.Var); err != nil {
			return "", fmt.Errorf("delivery.var: %w", err)
		}
		if _, clash := d.Config[del.Var]; clash {
			return "", fmt.Errorf("delivery.var: %q is also a config key", del.Var)
		}
		return "env " + del.Var, nil
	case ComponentDeliveryFile:
		if !ComponentFileDelivery {
			return "", errors.New("delivery.mode: file delivery is not available in this release")
		}
		if del.Host != "" || del.Header != "" || del.Format != "" || del.PlainHTTP || del.Var != "" {
			return "", errors.New("delivery: only file is part of file delivery")
		}
		if !componentFileTokenRE.MatchString(del.File) {
			return "", fmt.Errorf("delivery.file: %q must be a file name of lower-case letters, digits, '_', '.' and '-', "+
				"starting with a letter or digit, at most 63 characters", del.File)
		}
		return "file " + del.File, nil
	default:
		return "", fmt.Errorf("delivery.mode: unknown %q (want header, env or file)", del.Mode)
	}
}

// validHeaderHost checks a header delivery's host. The proxy keys a
// credential by bare host, so a port here would be refused when the proxy
// starts, and the credential goes to that host on its standard TLS port — so
// an egress entry admitting only another port would leave it never sent.
func validHeaderHost(host string, hosts []string) error {
	d, err := ParseDestination(host)
	switch {
	case err != nil || d.Wildcard:
		return fmt.Errorf("%q must be one host, not a wildcard", host)
	case d.Port != 0:
		return fmt.Errorf("%q must not carry a port: a header credential is sent to the host's standard TLS port", host)
	case d.String() != host:
		return fmt.Errorf("write %q as %q", host, d.String())
	}
	for _, h := range hosts {
		if e, err := ParseDestination(h); err == nil && !e.Wildcard && e.Host == d.Host && (e.Port == 0 || e.Port == 443) {
			return nil
		}
	}
	return fmt.Errorf("%q must be one of this component's hosts, listed bare or with :443 — the port a header credential is sent on", host)
}

// validComponentEnvName is the rule for a name written into the sandbox's
// environment, as a delivered secret's variable or as a config key.
// WARDYN_* names configure the sandbox harness itself, so authoring one would
// turn a component into a dispatch-config override.
func validComponentEnvName(name string) error {
	if len(name) > MaxComponentEnvNameBytes {
		return fmt.Errorf("must be at most %d bytes", MaxComponentEnvNameBytes)
	}
	if !componentEnvNameRE.MatchString(name) {
		return fmt.Errorf("%q must match [A-Z_][A-Z0-9_]*", name)
	}
	if strings.HasPrefix(name, "WARDYN_") {
		return fmt.Errorf("%q is reserved: WARDYN_* configures the sandbox itself", name)
	}
	if reservedComponentEnvName(name) {
		return fmt.Errorf("%q is reserved: it decides how programs in the sandbox start, or how they reach the network, and a component may not set it", name)
	}
	return nil
}

// reservedComponentEnvNames and reservedComponentEnvPrefixes are the variables
// no component may set, an organisation's or a person's, as a config key or as
// the variable a secret is delivered in. One closed list.
//
// The agent, the recorder that wraps it and the hold on its tool calls all
// start INSIDE the sandbox, under the environment dispatch composes. A
// variable that decides which program a name resolves to, what a runtime loads
// before its own code, or where a shell reads its startup from would let
// whoever wrote the component run their own code in those processes before
// the first held call. The proxy and trust variables are here for a plainer
// reason: Wardyn sets them, so a component's value would be dropped at
// dispatch, and refusing by name says so when the component is written.
var reservedComponentEnvNames = map[string]bool{
	// Which program runs, and where a shell or a session starts from.
	"PATH": true, "HOME": true, "SHELL": true, "BASH_ENV": true, "ENV": true, "PROMPT_COMMAND": true,
	// What a language runtime loads before the program's own code.
	"NODE_OPTIONS": true, "NODE_PATH": true,
	"PYTHONPATH": true, "PYTHONHOME": true, "PYTHONSTARTUP": true,
	"PERL5OPT": true, "PERL5LIB": true, "RUBYOPT": true, "RUBYLIB": true,
	// The helpers git runs, and the agent's own configuration directory.
	"GIT_EXEC_PATH": true, "GIT_SSH": true, "GIT_SSH_COMMAND": true, "GIT_PROXY_COMMAND": true,
	"CLAUDE_CONFIG_DIR": true,
	// The route to the proxy and the trust Wardyn installs for it.
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "REQUESTS_CA_BUNDLE": true, "CURL_CA_BUNDLE": true,
	"NODE_EXTRA_CA_CERTS": true, "GIT_SSL_CAINFO": true, "GIT_SSL_NO_VERIFY": true,
}

// The dynamic loader's variables, git's injected configuration, and the
// agent's own switches. WARDYN_ is refused beside them, with its own sentence.
var reservedComponentEnvPrefixes = []string{"LD_", "GIT_CONFIG_", "CLAUDE_CODE_"}

// reservedComponentEnvName reports whether name is one a component may not
// set (WARDYN_* aside, which validComponentEnvName refuses first).
func reservedComponentEnvName(name string) bool {
	if reservedComponentEnvNames[name] {
		return true
	}
	for _, p := range reservedComponentEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ValidComponentName checks a component's display name: 1 to
// MaxComponentNameRunes printable characters with no surrounding space, so
// two names that read the same are the same row.
func ValidComponentName(name string) error {
	n := utf8.RuneCountInString(name)
	if n == 0 || n > MaxComponentNameRunes {
		return fmt.Errorf("name: must be 1 to %d characters", MaxComponentNameRunes)
	}
	if !printable(name) || name != strings.TrimSpace(name) {
		return errors.New("name: must be printable text with no leading or trailing space")
	}
	return nil
}

// Validate checks a stored component: its name, and its definition as an org
// row (Owner "") or a person's.
func (c Component) Validate(validHost func(string) error) error {
	if err := ValidComponentName(c.Name); err != nil {
		return err
	}
	if err := c.Definition.Validate(validHost, c.Owner != ""); err != nil {
		return fmt.Errorf("definition.%w", err)
	}
	return nil
}

// Validate checks one attachment: exactly one of ID and Inline, and for an
// inline one its definition, always a person's, and optional label.
func (r ComponentRef) Validate(validHost func(string) error) error {
	switch {
	case (r.ID == nil) == (r.Inline == nil):
		return errors.New("give exactly one of id and inline")
	case r.ID != nil:
		if r.Name != "" {
			return errors.New("name: labels an inline component only")
		}
		return nil
	}
	if r.Name != "" {
		if err := ValidComponentName(r.Name); err != nil {
			return err
		}
	}
	if err := r.Inline.Validate(validHost, true); err != nil {
		return fmt.Errorf("inline.%w", err)
	}
	return nil
}

// ValidateComponentRefs checks a run request's components. Whether each one
// exists and may be attached is the caller's.
func ValidateComponentRefs(refs []ComponentRef, validHost func(string) error) error {
	if len(refs) > MaxComponentRefs {
		return fmt.Errorf("components: %d entries exceeds the %d-component limit", len(refs), MaxComponentRefs)
	}
	for i, r := range refs {
		if err := r.Validate(validHost); err != nil {
			return fmt.Errorf("components[%d]: %w", i, err)
		}
	}
	return nil
}

// printable reports whether s is valid UTF-8 made only of printable
// characters, which excludes line breaks and every other control character.
func printable(s string) bool {
	return utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}
