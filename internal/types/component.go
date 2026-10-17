// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/armscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
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
	MaxComponentRefs             = 16
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
	// CLI marks an org row as a CLI credential card (azure, aws or github).
	CLI *ComponentCLI `json:"cli,omitempty"`
	// MCP makes the component a remote MCP server, which a run's harnesses
	// reach only through the proxy's broker route (component_mcp.go).
	MCP *ComponentMCP `json:"mcp,omitempty"`
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

// ComponentRef attaches one component to a run: a stored one by ID, a run-only
// definition given inline (never stored as a component), or a built-in Git
// provider (Builtin; OD-4). Name labels an inline one.
type ComponentRef struct {
	ID     *uuid.UUID           `json:"id,omitempty"`
	Inline *ComponentDefinition `json:"inline,omitempty"`
	Name   string               `json:"name,omitempty"`
	// Builtin names a built-in Git provider component, github or azure_devops:
	// API and git-over-HTTPS access to the listed repositories without a
	// workspace, the agent cloning for itself. Org and Repos scope it, Access
	// narrows it.
	Builtin string `json:"builtin,omitempty"`
	// Org is the GitHub owner or the Azure DevOps organisation. Required with Builtin.
	Org string `json:"org,omitempty"`
	// Repos are owner/name (GitHub) or project/repository (Azure DevOps). GitHub
	// requires at least one; an Azure DevOps ref without any reaches every
	// repository the person can reach in Org.
	Repos []string `json:"repos,omitempty"`
	// Access is read or write; empty reads as read.
	Access string `json:"access,omitempty"`
	// MCPHarnesses are the included harnesses (tools[].id) an MCP component's
	// server is turned on for. Absent means every harness that can take it.
	MCPHarnesses []string `json:"mcp_harnesses,omitempty"`
}

// The built-in Git provider components a ComponentRef may name.
const (
	ComponentBuiltinGitHub      = "github"
	ComponentBuiltinAzureDevOps = "azure_devops"
)

// Built-in component access levels.
const (
	ComponentAccessRead  = "read"
	ComponentAccessWrite = "write"
)

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
	// MCPHarnesses are the harnesses an MCP component's server was turned on
	// for at launch. Not personal data: it survives an erasure, so a revive
	// still knows which harness reached which server.
	MCPHarnesses []string `json:"mcp_harnesses,omitempty"`
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
	if err := d.validateCLI(personDefined); err != nil {
		return err
	}
	if err := d.validateMCP(); err != nil {
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
	case r.Builtin != "":
		return r.validateBuiltin()
	case r.Org != "" || r.Repos != nil || r.Access != "":
		return errors.New("org, repos and access: scope a builtin component only")
	case (r.ID == nil) == (r.Inline == nil):
		return errors.New("give exactly one of id and inline")
	}
	if err := validMCPHarnesses(r.MCPHarnesses); err != nil {
		return err
	}
	if r.ID != nil {
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

// MaxComponentBuiltinRepos bounds the repositories one built-in component lists.
const (
	MaxComponentBuiltinRepos      = 64
	MaxComponentBuiltinFieldBytes = 256
)

// validateBuiltin checks a built-in Git provider reference's shape. That the
// person may attach it, and what it grants, is the caller's.
func (r ComponentRef) validateBuiltin() error {
	if r.ID != nil || r.Inline != nil || r.Name != "" || r.MCPHarnesses != nil {
		return errors.New("builtin: give it alone, without id, inline, name or mcp_harnesses")
	}
	if r.Builtin != ComponentBuiltinGitHub && r.Builtin != ComponentBuiltinAzureDevOps {
		return fmt.Errorf("builtin: %q is not one of %s, %s", r.Builtin, ComponentBuiltinGitHub, ComponentBuiltinAzureDevOps)
	}
	if r.Org == "" || len(r.Org) > MaxComponentBuiltinFieldBytes || !printable(r.Org) {
		return errors.New("org: name the organisation, in printable characters")
	}
	switch r.Access {
	case "", ComponentAccessRead, ComponentAccessWrite:
	default:
		return fmt.Errorf("access: %q is not one of %s, %s", r.Access, ComponentAccessRead, ComponentAccessWrite)
	}
	if r.Builtin == ComponentBuiltinGitHub && len(r.Repos) == 0 {
		return errors.New("repos: list at least one repository for github")
	}
	if len(r.Repos) > MaxComponentBuiltinRepos {
		return fmt.Errorf("repos: %d entries exceeds the %d-repository limit", len(r.Repos), MaxComponentBuiltinRepos)
	}
	for i, repo := range r.Repos {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") || len(repo) > MaxComponentBuiltinFieldBytes || !printable(repo) {
			return fmt.Errorf("repos[%d]: %q must be owner/name (project/repository for azure_devops)", i, repo)
		}
		if slices.Contains(r.Repos[:i], repo) {
			return fmt.Errorf("repos[%d]: %q is listed twice", i, repo)
		}
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

// CLI kinds a component row can carry. gcloud and kubectl are not built.
const (
	CLIKindAzure  = "azure"
	CLIKindAWS    = "aws"
	CLIKindGitHub = "github"
)

// CLISecretPrefix starts every person's CLI sign-in capture:
// wardyn-cli-<component id>-{sso,entra}.
const CLISecretPrefix = "wardyn-cli-"

// MaxCLIAzureSubscriptions bounds the subscriptions one Azure CLI row pins.
const MaxCLIAzureSubscriptions = 16

// ComponentCLI is the credential an org row gives a run's command-line tool.
// Kind names which of the three arms is set, and only that one is.
type ComponentCLI struct {
	Kind   string     `json:"kind"` // azure | aws | github
	Azure  *CLIAzure  `json:"azure,omitempty"`
	AWS    *CLIAWS    `json:"aws,omitempty"`
	GitHub *CLIGitHub `json:"github,omitempty"`
}

// CLIGitHub scopes the GitHub CLI to the run's repositories by capability.
type CLIGitHub struct {
	Capabilities []ghscope.Capability `json:"capabilities"`
}

// CLIGitHubRefused is the closed list of grantable capabilities a GitHub CLI
// row never carries: each changes a repository's or an organisation's
// settings, deletes packages, or administers Actions, and the CLI's token
// acts as the organisation's App rather than as the person.
var CLIGitHubRefused = []ghscope.Capability{ghscope.CapRepoAdmin, ghscope.CapOrgAdmin, ghscope.CapPackagesWrite, ghscope.CapActionsAdmin}

// CLIGitHubCapabilities is caps without anything a GitHub CLI token may not
// hold: a capability that is not grantable, or one CLIGitHubRefused names.
func CLIGitHubCapabilities(caps []ghscope.Capability) []ghscope.Capability {
	return slices.DeleteFunc(slices.Clone(caps), func(c ghscope.Capability) bool {
		return !c.Grantable() || slices.Contains(CLIGitHubRefused, c)
	})
}

// CLIAWS pins one IAM Identity Center account and permission set.
type CLIAWS struct {
	StartURL  string `json:"start_url"`  // the access portal, https
	SSORegion string `json:"sso_region"` // where the portal lives
	AccountID string `json:"account_id"` // 12 digits
	RoleName  string `json:"role_name"`  // the permission set name
	Region    string `json:"region"`     // the CLI's default region
}

// CLIAzure pins subscriptions and whether the CLI may change them.
type CLIAzure struct {
	Subscriptions []string `json:"subscriptions"` // 1..MaxCLIAzureSubscriptions GUIDs
	Access        string   `json:"access"`        // read (GET and HEAD only) | write
}

// SubscriptionIDs is the row's subscriptions as Azure compares them: lowercase
// GUIDs, in the row's order. An entry that is not a GUID is "".
func (a CLIAzure) SubscriptionIDs() []string {
	ids := make([]string, len(a.Subscriptions))
	for i, sub := range a.Subscriptions {
		ids[i], _ = armscope.Subscription(sub)
	}
	return ids
}

var (
	cliAWSAccountRE = regexp.MustCompile(`^[0-9]{12}$`)
	cliAWSRegionRE  = regexp.MustCompile(`^[a-z]{2}(-[a-z0-9]+)+-[0-9]$`)
	cliAWSRoleRE    = regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]{1,64}$`)
)

// validateCLI checks the cli block: an org row only, carrying no secrets, with
// its own kind's arm and no other.
func (d ComponentDefinition) validateCLI(personDefined bool) error {
	c := d.CLI
	if c == nil {
		return nil
	}
	if personDefined {
		return errors.New("cli: only an organisation component can carry a CLI credential")
	}
	if len(d.Secrets) > 0 {
		return errors.New("cli: a CLI row carries no secrets; its credential is the run's")
	}
	arms := map[string]bool{CLIKindAzure: c.Azure != nil, CLIKindAWS: c.AWS != nil, CLIKindGitHub: c.GitHub != nil}
	have, ok := arms[c.Kind]
	if !ok {
		return fmt.Errorf("cli.kind: %q is not azure, aws or github", c.Kind)
	}
	if !have {
		return fmt.Errorf("cli.%s: required for kind %q", c.Kind, c.Kind)
	}
	for kind, set := range arms {
		if set && kind != c.Kind {
			return fmt.Errorf("cli.%s: not allowed for kind %q", kind, c.Kind)
		}
	}
	switch c.Kind {
	case CLIKindAzure:
		return c.Azure.validate()
	case CLIKindAWS:
		return c.AWS.validate()
	}
	return c.GitHub.validate()
}

func (g *CLIGitHub) validate() error {
	if len(g.Capabilities) == 0 {
		return errors.New("cli.github.capabilities: at least one is required")
	}
	for i, cp := range g.Capabilities {
		if !cp.Grantable() {
			return fmt.Errorf("cli.github.capabilities[%d]: %q is not grantable; want one of: %s", i, cp, ghscope.GrantableCapabilityList())
		}
		if slices.Contains(CLIGitHubRefused, cp) {
			return fmt.Errorf("cli.github.capabilities[%d]: %q is not available to the GitHub CLI", i, cp)
		}
		if slices.Contains(g.Capabilities[:i], cp) {
			return fmt.Errorf("cli.github.capabilities[%d]: %q is listed twice", i, cp)
		}
	}
	return nil
}

// cliAWSPortalDomain is the AWS access portal's domain. The start URL is
// written into the sandbox's ~/.aws/config and the person signs in at it, so a
// row may name only an IAM Identity Center portal, never any https host.
const cliAWSPortalDomain = ".awsapps.com"

func (a *CLIAWS) validate() error {
	u, err := url.Parse(a.StartURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" ||
		!strings.HasSuffix(u.Hostname(), cliAWSPortalDomain) || len(u.Hostname()) == len(cliAWSPortalDomain) ||
		strings.ContainsAny(a.StartURL, " \t\r\n") {
		return errors.New("cli.aws.start_url: must be your AWS access portal URL, https://<name>.awsapps.com/start")
	}
	switch {
	case !cliAWSRegionRE.MatchString(a.SSORegion):
		return fmt.Errorf("cli.aws.sso_region: %q is not a region", a.SSORegion)
	case !cliAWSAccountRE.MatchString(a.AccountID):
		return errors.New("cli.aws.account_id: must be 12 digits")
	case !cliAWSRoleRE.MatchString(a.RoleName):
		return fmt.Errorf("cli.aws.role_name: %q is not a permission set name", a.RoleName)
	case !cliAWSRegionRE.MatchString(a.Region):
		return fmt.Errorf("cli.aws.region: %q is not a region", a.Region)
	}
	return nil
}

func (a *CLIAzure) validate() error {
	if n := len(a.Subscriptions); n == 0 || n > MaxCLIAzureSubscriptions {
		return fmt.Errorf("cli.azure.subscriptions: must list 1 to %d subscriptions", MaxCLIAzureSubscriptions)
	}
	// Azure compares a subscription id without regard to case, so two
	// spellings of one id are one subscription listed twice.
	ids := a.SubscriptionIDs()
	for i, sub := range a.Subscriptions {
		if ids[i] == "" {
			return fmt.Errorf("cli.azure.subscriptions[%d]: %q is not a subscription GUID", i, sub)
		}
		if slices.Contains(ids[:i], ids[i]) {
			return fmt.Errorf("cli.azure.subscriptions[%d]: %q is listed twice", i, sub)
		}
	}
	if a.Access != ComponentAccessRead && a.Access != ComponentAccessWrite {
		return fmt.Errorf("cli.azure.access: %q is not read or write", a.Access)
	}
	return nil
}

// ComponentMCPTransportHTTP is the one MCP transport a component may use:
// streamable HTTP.
const ComponentMCPTransportHTTP = "http"

// Bounds on an MCP component and on what one reference turns it on for.
const (
	MaxComponentMCPURLBytes  = 2048
	MaxComponentMCPHarnesses = 8
)

// ComponentMCP is a remote MCP server. The run's harnesses never reach URL
// itself: each is configured with the proxy's broker route, which adds the
// server's header credential, applies the run's tool rules to its tools and is
// the only network path to the server.
type ComponentMCP struct {
	// URL is the server's streamable HTTP address: https, no userinfo, its
	// host one of the component's hosts.
	URL       string `json:"url"`
	Transport string `json:"transport"` // http
}

// harnessIDRE is a harness id as tools[].id names one.
var harnessIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// validateMCP checks the mcp block. Its credential goes to the server and never
// into the sandbox, so its only secret mode is a header to the server's own
// host, over TLS.
func (d ComponentDefinition) validateMCP() error {
	m := d.MCP
	if m == nil {
		return nil
	}
	if d.CLI != nil {
		return errors.New("mcp: a component is an MCP server or a CLI credential, not both")
	}
	switch m.Transport {
	case ComponentMCPTransportHTTP:
	case "sse":
		return errors.New("mcp.transport: servers that use SSE are not supported yet; use the server's streamable HTTP address")
	default:
		return fmt.Errorf("mcp.transport: %q is not http", m.Transport)
	}
	host, err := MCPURLDestination(m.URL)
	if err != nil {
		return fmt.Errorf("mcp.url: %w", err)
	}
	if !slices.ContainsFunc(d.Hosts, func(h string) bool { return mcpHostAdmits(h, host) }) {
		return fmt.Errorf("mcp.url: its host %q must be one of this component's hosts", host.String())
	}
	for i, s := range d.Secrets {
		del := s.Delivery
		switch {
		case del.Mode != ComponentDeliveryHeader:
			return fmt.Errorf("secrets[%d].delivery.mode: an MCP server's secret is sent to the server as a header, never into the sandbox", i)
		case del.Host != host.Host:
			return fmt.Errorf("secrets[%d].delivery.host: an MCP server's secret goes to the server's host %q", i, host.Host)
		case del.PlainHTTP:
			return fmt.Errorf("secrets[%d].delivery.plain_http: an MCP server's secret is only ever sent over TLS", i)
		}
	}
	return nil
}

// MCPURLDestination checks an MCP server URL and returns its host and port as
// a destination (Port 0 for the standard https port).
func MCPURLDestination(raw string) (Destination, error) {
	if len(raw) > MaxComponentMCPURLBytes || !printable(raw) {
		return Destination{}, fmt.Errorf("must be printable text of at most %d bytes", MaxComponentMCPURLBytes)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return Destination{}, errors.New("must be an https URL with a host, and no credentials or fragment")
	}
	d, err := ParseDestination(u.Host)
	if err != nil || d.Wildcard {
		return Destination{}, fmt.Errorf("%q is not one host", u.Host)
	}
	if d.Port == 443 {
		d.Port = 0
	}
	return d, nil
}

// mcpHostAdmits reports whether a component host entry names the MCP URL's
// host on its port, the standard port written bare or as :443.
func mcpHostAdmits(entry string, url Destination) bool {
	e, err := ParseDestination(entry)
	if err != nil || e.Wildcard || e.Host != url.Host {
		return false
	}
	return e.Port == url.Port || (url.Port == 0 && e.Port == 443)
}

// validMCPHarnesses checks the harness list of one reference: harness ids,
// each once.
func validMCPHarnesses(ids []string) error {
	if len(ids) > MaxComponentMCPHarnesses {
		return fmt.Errorf("mcp_harnesses: %d entries exceeds the %d-harness limit", len(ids), MaxComponentMCPHarnesses)
	}
	for i, id := range ids {
		if !harnessIDRE.MatchString(id) {
			return fmt.Errorf("mcp_harnesses[%d]: %q is not a harness id", i, id)
		}
		if slices.Contains(ids[:i], id) {
			return fmt.Errorf("mcp_harnesses[%d]: %q is listed twice", i, id)
		}
	}
	return nil
}
