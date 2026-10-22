// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/cjohnstoniv/wardyn/internal/certbundle"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// stdinIsTerminal reports whether `wardyn ca add` may ask. A var so a test can
// play the person at the prompt.
var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// systemRoots is the pool an intermediate's issuer may come from; a var so a
// test can stand in a system root.
var systemRoots = func() *x509.CertPool {
	p, err := x509.SystemCertPool()
	if err != nil {
		return nil
	}
	return p
}

const (
	msgTOFU    = "No fingerprint is published for this organisation: you are trusting this bundle on first use."
	msgCompare = "Compare each fingerprint above with the one the console shows under Get the CLI."
)

func caCmd(client func() *sdk.Client) *cobra.Command {
	ca := subcommandGroup(&cobra.Command{
		Use:   "ca",
		Short: "Trust anchors for this CLI's own connection to an organisation (never system-wide, never a sandbox)",
	})
	ca.AddCommand(caAddCmd(client), caListCmd(), caVerifyCmd(client), caRemoveCmd())
	return ca
}

func caAddCmd(client func() *sdk.Client) *cobra.Command {
	var org string
	var fingerprints []string
	cmd := &cobra.Command{
		Use:   "add <file|https-url>",
		Short: "Trust a CA bundle for one organisation's connection only",
		Long: "Trust a PEM bundle (up to 64 KiB) for this CLI's connection to one organisation (--org, default the CLI's URL).\n" +
			"Every certificate is printed with its SHA-256 fingerprint; compare them with the console's Get the CLI.\n" +
			"Without a terminal, pass --fingerprint for every CA certificate.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if org == "" {
				org = client().BaseURL
			}
			return caAdd(cmd.OutOrStdout(), cmd.InOrStdin(), args[0], org, fingerprints, time.Now())
		},
	}
	cmd.Flags().StringVar(&org, "org", "", "the organisation URL these anchors are trusted for (default: the CLI's URL)")
	cmd.Flags().StringArrayVar(&fingerprints, "fingerprint", nil, "sha256:<hex> of a CA certificate the bundle must hold (repeat for each)")
	return cmd
}

func caAdd(out io.Writer, in io.Reader, src, org string, given []string, now time.Time) error {
	path, origin, err := anchorFile(org)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(origin, "https://") {
		return fmt.Errorf("nothing was added: --org %s is not https://, and an anchor only applies to a TLS connection", org)
	}
	raw, err := readBundle(src)
	if err != nil {
		return err
	}
	entries, err := certbundle.Parse(raw, now)
	if err != nil {
		return err
	}
	for _, e := range entries {
		printCert(out, e.Cert)
		switch {
		case e.Problem != "":
			fmt.Fprintf(out, "  not added: %s\n", e.Problem)
		case e.ExpiresSoon:
			fmt.Fprintln(out, "  expires within 30 days")
		}
	}
	usable := certbundle.Usable(entries)
	if len(usable) == 0 {
		return errors.New("nothing was added: the bundle holds no usable CA certificate")
	}
	existing, err := storedAnchors(origin)
	if err != nil {
		return err
	}
	if err := certbundle.CheckChain(usable, existing, systemRoots(), now); err != nil {
		return fmt.Errorf("nothing was added: %w", err)
	}
	if err := confirmTrust(out, in, origin, usable, given); err != nil {
		return err
	}
	merged := existing
	for _, c := range usable {
		if !containsCert(merged, c) {
			merged = append(merged, c)
		}
	}
	if err := writeAnchors(path, certbundle.Encode(merged)); err != nil {
		return err
	}
	fmt.Fprintf(out, "Added %d CA certificate(s), trusted only for this CLI's connection to %s: %s\n", len(usable), origin, path)
	return nil
}

// confirmTrust is CA rule 2: every CA's fingerprint matches a --fingerprint,
// or a person at a terminal compares and accepts. Nothing is added silently.
func confirmTrust(out io.Writer, in io.Reader, origin string, usable []*x509.Certificate, given []string) error {
	if len(given) > 0 {
		want := map[string]bool{}
		for _, g := range given {
			fp, err := certbundle.NormalizeFingerprint(g)
			if err != nil {
				return err
			}
			want[fp] = true
		}
		have := map[string]bool{}
		for _, c := range usable {
			fp := certbundle.Fingerprint(c)
			if !want[fp] {
				return fmt.Errorf("nothing was added: %s (%s) matches no --fingerprint", c.Subject, fp)
			}
			have[fp] = true
		}
		for fp := range want {
			if !have[fp] {
				return fmt.Errorf("nothing was added: --fingerprint %s matches no CA certificate in the bundle", fp)
			}
		}
		return nil
	}
	if !stdinIsTerminal() {
		return errors.New("nothing was added: without a terminal, pass --fingerprint sha256:<hex> for every CA certificate")
	}
	published, err := publishedFingerprints(origin, usable)
	switch {
	case err != nil:
		fmt.Fprintf(out, "%s (The published fingerprints could not be read: %v.)\n", msgCompare, err)
	case len(published) == 0:
		fmt.Fprintln(out, msgTOFU)
	default:
		fmt.Fprintln(out, msgCompare)
	}
	fmt.Fprintf(out, "Trust %d CA certificate(s) for %s only? [y/N] ", len(usable), origin)
	line, _ := bufio.NewReader(in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("nothing was added")
	}
	return nil
}

// publishedFingerprints is /healthz.org_ca_fingerprints, read through the
// anchors being added. It is fetched over the connection it describes, so it
// only chooses which line the prompt prints; the person's comparison with the
// console is the check.
func publishedFingerprints(origin string, extra []*x509.Certificate) ([]string, error) {
	resp, err := orgClientTrusting(origin, 30*time.Second, extra).Get(origin + "/healthz")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/healthz answered %s", resp.Status)
	}
	var body struct {
		Fingerprints []string `json:"org_ca_fingerprints"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	return body.Fingerprints, nil
}

// readBundle is src's bytes, a file or an https URL fetched through the
// system roots (and that host's own anchors). A fetch that fails verification
// is never retried insecurely.
func readBundle(src string) ([]byte, error) {
	var r io.Reader
	if strings.Contains(src, "://") {
		u, err := url.Parse(src)
		if err != nil || u.Scheme != "https" {
			return nil, fmt.Errorf("%q: a bundle URL must be https://", src)
		}
		resp, err := orgClient(u.Scheme+"://"+u.Host, 30*time.Second).Get(src)
		if err != nil {
			return nil, fmt.Errorf("fetching the bundle: %w (download it in your browser and pass the file instead)", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetching the bundle: %s answered %s", src, resp.Status)
		}
		r = resp.Body
	} else {
		f, err := os.Open(src)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	raw, err := io.ReadAll(io.LimitReader(r, certbundle.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// writeAnchors replaces path with pemBytes, mode 0400 in a 0700 directory.
func writeAnchors(path string, pemBytes []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".add-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(pemBytes); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o400); err != nil {
		return err
	}
	// A read-only target cannot be replaced on Windows; make it writable first.
	_ = os.Chmod(path, 0o600)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	_, _, err = certbundle.LoadFile(path, time.Now())
	return err
}

func containsCert(set []*x509.Certificate, c *x509.Certificate) bool {
	for _, o := range set {
		if o.Equal(c) {
			return true
		}
	}
	return false
}

func printCert(out io.Writer, c *x509.Certificate) {
	fmt.Fprintf(out, "%s\n  issuer: %s\n  not after: %s\n  %s\n",
		c.Subject, c.Issuer, c.NotAfter.UTC().Format(time.RFC3339), certbundle.Fingerprint(c))
}

func caListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show the stored anchors per organisation, with each file's mode check",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return caList(cmd.OutOrStdout(), time.Now())
		},
	}
}

func caList(out io.Writer, now time.Time) error {
	dir, err := anchorDir()
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.pem"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintln(out, "No anchors are stored. Add one with `wardyn ca add <file> --org <url>`.")
		return nil
	}
	sort.Strings(files)
	for _, f := range files {
		fmt.Fprintf(out, "%s\n  file: %s\n", strings.TrimSuffix(filepath.Base(f), ".pem"), f)
		certs, warnings, err := certbundle.LoadFile(f, now)
		for _, w := range warnings {
			fmt.Fprintf(out, "  warning: %s\n", w)
		}
		if err != nil {
			fmt.Fprintf(out, "  refused: %v\n", err)
			continue
		}
		fmt.Fprintln(out, "  mode: ok")
		for _, c := range certs {
			fmt.Fprintf(out, "  %s\n    expires: %s\n    %s\n", c.Subject, c.NotAfter.UTC().Format(time.RFC3339), certbundle.Fingerprint(c))
		}
	}
	return nil
}

func caRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <url>",
		Short: "Remove the anchors stored for one organisation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, origin, err := anchorFile(args[0])
			if err != nil {
				return err
			}
			if err := os.Remove(path); errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(cmd.OutOrStdout(), "No anchors are stored for %s.\n", origin)
				return nil
			} else if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed the anchors for %s: %s\n", origin, path)
			return nil
		},
	}
}

func caVerifyCmd(client func() *sdk.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "verify [<url>]",
		Short: "Make one TLS handshake to the organisation through the proxy in effect and name what signed it",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			} else {
				target = client().BaseURL
			}
			return caVerify(cmd.OutOrStdout(), target)
		},
	}
}

// caVerify makes the handshake the real connection makes (orgClient: same
// proxy route, same anchors) and prints the chain presented and its signer.
func caVerify(out io.Writer, target string) error {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" {
		return fmt.Errorf("%q: verify needs an https:// URL", target)
	}
	req, err := http.NewRequest(http.MethodHead, target, nil)
	if err != nil {
		return err
	}
	resp, err := orgClient(u.Scheme+"://"+u.Host, 30*time.Second).Do(req)
	if err != nil {
		var cve *tls.CertificateVerificationError
		var ua x509.UnknownAuthorityError
		if errors.As(err, &cve) && errors.As(err, &ua) && len(cve.UnverifiedCertificates) > 0 {
			printChain(out, cve.UnverifiedCertificates)
			last := cve.UnverifiedCertificates[len(cve.UnverifiedCertificates)-1]
			fmt.Fprintf(out, "signed by: unknown authority %s: add it with `wardyn ca add`\n", last.Issuer)
			return errors.New("the chain is signed by an unknown authority")
		}
		return err
	}
	resp.Body.Close()
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 || len(resp.TLS.VerifiedChains) == 0 {
		return errors.New("no TLS chain was presented")
	}
	presented := resp.TLS.PeerCertificates
	printChain(out, presented)
	inter := x509.NewCertPool()
	for _, c := range presented[1:] {
		inter.AddCert(c)
	}
	if _, err := presented[0].Verify(x509.VerifyOptions{DNSName: u.Hostname(), Intermediates: inter, Roots: systemRoots()}); err == nil {
		fmt.Fprintln(out, "signed by: a system root")
		return nil
	}
	chain := resp.TLS.VerifiedChains[0]
	fmt.Fprintf(out, "signed by: the organisation's CA, added with `wardyn ca add`: this connection is intercepted by %s\n", chain[len(chain)-1].Subject)
	fmt.Fprintln(out, "(interception is a heuristic: the chain verifies only through a root that is not a system root)")
	return nil
}

func printChain(out io.Writer, chain []*x509.Certificate) {
	for _, c := range chain {
		fmt.Fprintf(out, "presented: %s\n  issuer: %s\n  not after: %s\n", c.Subject, c.Issuer, c.NotAfter.UTC().Format(time.RFC3339))
	}
}
