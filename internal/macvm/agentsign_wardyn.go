// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"cmp"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm/machosign"
)

// Which signer signed the guest agent a tools update pushed (ToolsRecord.AgentSigner). They are tried in this
// order: Wardyn's own, then codesign, then none (the build as it is, ad hoc).
const (
	// AgentSignerWardyn: signed in this process (machosign) with this Mac's identity; no keychain or trust.
	AgentSignerWardyn = "wardyn"
	// AgentSignerCodesign: signed by codesign with this Mac's identity, through a temporary keychain.
	AgentSignerCodesign = "codesign"
	// AgentSignerAdHoc: neither signed it, so the build went in signed ad hoc.
	AgentSignerAdHoc = "ad_hoc"
)

// AgentSignRecord is which signer signed the pushed guest agent, and why each signer tried before it failed,
// as "<signer>: <error>" with no path. It is in both the tools update's result and tools.json.
type AgentSignRecord struct {
	AgentSigner     string   `json:"agent_signer,omitempty"`
	AgentSignErrors []string `json:"agent_sign_errors,omitempty"`
}

// AgentSignError is a signing that no signer finished: Errors says why each failed, path-free, for tools.json;
// Error has the full text, paths included, for this Mac's log.
type AgentSignError struct {
	Errors []string
	full   []string
}

// add records why signer failed: err in full, and path-free.
func (e *AgentSignError) add(signer string, err error) {
	prefix := ""
	if signer != "" {
		prefix = signer + ": "
	}
	e.Errors = append(e.Errors, prefix+pathFree(err.Error()))
	e.full = append(e.full, prefix+err.Error())
}

func (e *AgentSignError) Error() string {
	msgs := e.full
	if len(msgs) == 0 {
		msgs = e.Errors
	}
	return "macvm: the guest agent was not signed: " + strings.Join(msgs, "; ")
}

// signInProcess signs path's build at cp in this process, with the identity's key read from Dir.
func (s *HostAgentSigner) signInProcess(cert *x509.Certificate, path, cp string) error {
	key, err := readAgentSigningKey(filepath.Join(s.Dir, agentSigningKey), cert)
	if err != nil {
		return err
	}
	bin, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := machosign.Sign(bin, AgentSigningID, cert, key)
	if err != nil {
		return err
	}
	return os.WriteFile(cp, out, 0o755)
}

// readAgentSigningKey is the identity's key at path, checked to be cert's.
func readAgentSigningKey(path string, cert *x509.Certificate) (*rsa.PrivateKey, error) {
	if err := checkAgentSigningKey(path, cert); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("the signing key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the signing key is not RSA")
	}
	return rk, nil
}

// keychainList is the keychains `security list-keychains` printed, one quoted path a line.
func keychainList(out string) []string {
	var list []string
	for _, line := range strings.Split(out, "\n") {
		if p := strings.Trim(strings.TrimSpace(line), `"`); p != "" {
			list = append(list, p)
		}
	}
	return list
}

// restoreKeychains puts back the login's keychain search list as it read before the signing. It runs under
// its own bounded context, as deleteKeychain does, so a cancelled update still restores it. Setting the user
// list writes the login's preferences only and raises no prompt. A failure is logged: deleting the keychain
// next also takes it off the list.
func (s *HostAgentSigner) restoreKeychains(ctx context.Context, exec func(context.Context, []string) ([]byte, error), list []string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if out, err := exec(rctx, append([]string{"/usr/bin/security", "list-keychains", "-d", "user", "-s"}, list...)); err != nil {
		cmp.Or(s.Log, slog.Default()).Warn("macvm: the keychain search list was not restored after signing the guest agent", "err", err.Error(), "out", outputTail(string(out)))
	}
}

// pathFree is a signing error as tools.json and doctor carry it: each absolute or home-relative path (the agent
// build, the temp directory, this Mac's signing directory, a keychain that security names) reads "<path>". The
// full text stays in the update's log.
func pathFree(msg string) string { return pathToken.ReplaceAllString(msg, "${1}<path>") }

// pathToken is a path at the start of a word: "/" or "~/" up to a space, quote, comma, semicolon, colon or ")".
var pathToken = regexp.MustCompile(`(^|[\s"'(=])~?/[^\s"',;:)]*`)

// agentToPush is the guest agent build stage 2 pushes: o.Signer's signed copy, or the build as it is
// (signed ad hoc) when there is no signer or every signer failed, with the signer and its errors to record.
// A failure is logged and never stops the update: the signature is not a security boundary, and refusing
// would only leave the base's tools behind.
func agentToPush(ctx context.Context, o ToolsOptions) (path string, signed *SignedAgent, rec AgentSignRecord) {
	rec.AgentSigner = AgentSignerAdHoc
	if o.Signer == nil {
		return o.Agent.Path, nil, rec
	}
	sa, err := o.Signer.SignAgent(ctx, o.Agent.Path)
	if err != nil {
		o.Log.Warn("macvm: the guest agent was not signed on this Mac; pushing it signed ad hoc, so macOS drops its Full Disk Access at this update", "err", err.Error())
		var se *AgentSignError
		if errors.As(err, &se) {
			rec.AgentSignErrors = se.Errors
		} else {
			rec.AgentSignErrors = []string{fmt.Sprint(err)}
		}
		return o.Agent.Path, nil, rec
	}
	return sa.Path, &sa, AgentSignRecord{AgentSigner: sa.Signer, AgentSignErrors: sa.Errors}
}
