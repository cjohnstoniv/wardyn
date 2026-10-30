// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// FlagBaseFile is prepare's and update-tools' install list (the runner setting macos_vm_base_file).
const FlagBaseFile = "base-file"

// Base item kinds.
const (
	ItemKindPkg     = "pkg"
	ItemKindApp     = "app"
	ItemKindFile    = "file"
	ItemKindProfile = "profile"
	ItemKindCA      = "ca"
)

// Item states: installed, a profile staged for the person's approval in macos-vm edit-base, or a CA the
// guest trusts or, read back, does not (then the person trusts it by hand in edit-base).
const (
	ItemInstalled  = "installed"
	ItemStaged     = "staged"
	ItemTrusted    = "trusted"
	ItemNotTrusted = "not_trusted"
)

// profileID is a configuration profile's PayloadIdentifier.
var profileID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

// maxBaseList bounds the install list file.
const maxBaseList = 1 << 20

// BaseList is the operator's install list, the file the locked runner setting macos_vm_base_file names:
// the items stage 2 installs into the base as admin, at prepare and at every tools update. Each item's
// file is on this Mac, pinned by its SHA-256, staged in the guest in a root-only directory and installed
// with fixed argv forms (guestagent itemArgvAllowed).
type BaseList struct {
	Version int        `json:"version"`
	Items   []BaseItem `json:"items"`
}

// BaseItem is one item of the list.
type BaseItem struct {
	Kind string `json:"kind"`
	// Name is the item's own name (guestagent.ItemNamePattern), unique in the list.
	Name string `json:"name"`
	// Path is the item's file on this Mac, and SHA256 the hash it must have.
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	// Dest is a file item's path in the guest (guestagent.ItemDestAllowed).
	Dest string `json:"dest,omitempty"`
	// App is the bundle an app item's .dmg holds ("Name.app"); an app item's .zip holds its bundle at the
	// top and needs none.
	App string `json:"app,omitempty"`
	// Identifier is a profile item's PayloadIdentifier, which `profiles list` reads back once it is
	// installed.
	Identifier string `json:"identifier,omitempty"`
	// SecretFiles are a pkg item's secret files, such as an EDR's licence.
	SecretFiles []SecretFile `json:"secret_files,omitempty"`
}

// SecretFile is a file on this Mac, mode 0600 and pinned by its SHA-256, that stage 2 pushes as Name
// beside the item's pkg, root's and 0600, for its installer to read, and removes with the item's stage
// directory after the install, on every path. Its contents are never in an argv, a log, tools.json or
// the caps; the list's digest covers its pin only.
type SecretFile struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// LoadBaseList reads and checks the install list at path.
func LoadBaseList(path string) (BaseList, error) {
	f, err := os.Open(path)
	if err != nil {
		return BaseList{}, fmt.Errorf("macvm: the macOS base install list: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBaseList+1))
	if err != nil {
		return BaseList{}, fmt.Errorf("macvm: the macOS base install list: %w", err)
	}
	if len(b) > maxBaseList {
		return BaseList{}, fmt.Errorf("macvm: the macOS base install list %s is over %d bytes", path, maxBaseList)
	}
	l, err := ParseBaseList(b)
	if err != nil {
		return BaseList{}, fmt.Errorf("macvm: the macOS base install list %s: %w", path, err)
	}
	return l, nil
}

// ParseBaseList parses an install list and checks every item, refusing an unknown field, kind or version.
func ParseBaseList(b []byte) (BaseList, error) {
	var l BaseList
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return BaseList{}, err
	}
	if l.Version != 1 {
		return BaseList{}, fmt.Errorf("version %d is not 1", l.Version)
	}
	seen := map[string]bool{}
	for i, it := range l.Items {
		if err := it.check(); err != nil {
			return BaseList{}, fmt.Errorf("item %d (%q): %w", i+1, it.Name, err)
		}
		if seen[it.Name] {
			return BaseList{}, fmt.Errorf("item %d: the name %q is used twice", i+1, it.Name)
		}
		seen[it.Name] = true
	}
	return l, nil
}

func (it BaseItem) check() error {
	switch {
	case !guestagent.ItemNamePattern.MatchString(it.Name):
		return errors.New("a name is lower-case letters, digits, '.', '_' and '-', at most 64")
	case !filepath.IsAbs(it.Path):
		return errors.New("path must be an absolute path on this Mac")
	case !sha256Hex.MatchString(it.SHA256):
		return errors.New("sha256 must be 64 lower-case hex digits")
	}
	ext := strings.ToLower(filepath.Ext(it.Path))
	if it.Kind != ItemKindProfile && it.Identifier != "" {
		return errors.New("only a profile item takes an identifier")
	}
	if err := it.checkSecrets(); err != nil {
		return err
	}
	switch it.Kind {
	case ItemKindPkg:
		if ext != ".pkg" || it.Dest != "" || it.App != "" {
			return errors.New("a pkg item's path ends in .pkg, and it takes no dest or app")
		}
	case ItemKindApp:
		if it.Dest != "" || !(ext == ".zip" && it.App == "" || ext == ".dmg" && guestagent.ItemAppPattern.MatchString(it.App)) {
			return errors.New("an app item is a .zip of its bundle, or a .dmg with app naming the bundle in it (Name.app); it takes no dest")
		}
	case ItemKindFile:
		if it.App != "" {
			return errors.New("a file item takes no app")
		}
		if err := guestagent.ItemDestAllowed(it.Dest); err != nil {
			return fmt.Errorf("dest %w", err)
		}
	case ItemKindProfile:
		if ext != ".mobileconfig" || it.Dest != "" || it.App != "" || !profileID.MatchString(it.Identifier) {
			return errors.New("a profile item's path ends in .mobileconfig, and it takes its PayloadIdentifier as identifier and no dest or app")
		}
	case ItemKindCA:
		if !slices.Contains([]string{".pem", ".cer", ".crt", ".der"}, ext) || it.Dest != "" || it.App != "" {
			return errors.New("a ca item is one certificate, its path ending in .pem, .cer, .crt or .der, and it takes no dest or app")
		}
	default:
		return fmt.Errorf("kind %q is not one of pkg, app, file, profile, ca", it.Kind)
	}
	return nil
}

func (it BaseItem) checkSecrets() error {
	if len(it.SecretFiles) > 0 && it.Kind != ItemKindPkg {
		return errors.New("only a pkg item takes secret_files")
	}
	seen := map[string]bool{}
	for _, sf := range it.SecretFiles {
		switch {
		case !filepath.IsAbs(sf.Path) || !sha256Hex.MatchString(sf.SHA256):
			return errors.New("a secret file has an absolute path on this Mac and its sha256")
		case !guestagent.ItemSecretPattern.MatchString(sf.Name) || sf.Name == guestagent.ItemPkg || sf.Name == guestagent.ItemMount || seen[sf.Name]:
			return fmt.Errorf("a secret file's name %q is letters, digits, '.', '_' and '-', not item.pkg or mnt, and used once", sf.Name)
		}
		seen[sf.Name] = true
	}
	return nil
}

// Digest is the list's digest, which tools.json records: a tools update runs when it changes. It is a
// hash of the parsed items, pins included; "" for no items.
func (l BaseList) Digest() string {
	if len(l.Items) == 0 {
		return ""
	}
	b, _ := json.Marshal(l.Items)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ItemsChanged says whether the base in baseDir records an install list digest other than digest; a base
// with no tools.json was built with no list.
func ItemsChanged(baseDir, digest string) bool {
	rec, _ := ReadToolsRecord(baseDir)
	return rec.ItemsDigest != digest
}

// ItemRecord is one item as stage 2 left it, in tools.json: never a secret, and never a path on this Mac.
type ItemRecord struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	State  string `json:"state"`
	// Identifier is a profile's PayloadIdentifier.
	Identifier string `json:"identifier,omitempty"`
	// Fingerprint and SHA1 are a CA's SHA-256 and SHA-1 fingerprints (of its DER); a tools update removes a
	// CA the list no longer has by its SHA-1.
	Fingerprint string `json:"fingerprint,omitempty"`
	SHA1        string `json:"sha1,omitempty"`
}

// installItems installs the list's items in order, after the harnesses and before the self-test, which
// then runs on the base as a run gets it. An item that fails stops stage 2: prepare publishes no base, and
// a tools update keeps the old one. Its command's output is not kept, since an installer's may echo what
// it was given.
func installItems(ctx context.Context, g ToolsGuest, o ToolsOptions, previous []ItemRecord, res *ToolsResult) error {
	res.ItemsDigest = o.Items.Digest()
	if err := removeRotatedCAs(ctx, g, o.Items, previous); err != nil {
		return err
	}
	if len(o.Items.Items) == 0 {
		return nil
	}
	if err := runItem(ctx, g, "/bin/mkdir", "-p", "-m", "0700", guestagent.MacItemStageDir); err != nil {
		return fmt.Errorf("macvm: the base items' stage directory: %w", err)
	}
	for _, it := range o.Items.Items {
		rec, err := installItem(ctx, g, it)
		if err != nil {
			return fmt.Errorf("macvm: base item %s (%s): %w", it.Name, it.Kind, err)
		}
		res.Items = append(res.Items, rec)
	}
	return nil
}

// listProfiles is the identifiers `profiles list` names; ok is false when it did not answer.
func listProfiles(ctx context.Context, g ToolsGuest) (installed map[string]bool, ok bool, err error) {
	out, exit, err := g.Run(ctx, []string{"/usr/bin/profiles", "list"})
	if err != nil || exit != 0 {
		return nil, false, err
	}
	installed = map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if _, id, found := strings.Cut(line, "attribute: profileIdentifier:"); found {
			installed[strings.TrimSpace(id)] = true
		}
	}
	return installed, true, nil
}

// readProfiles marks each profile item installed when installed names it, else staged. A profile is
// staged at prepare and approved by the person in macos-vm edit-base: macOS installs one only with that
// approval, or from MDM.
func readProfiles(items []ItemRecord, installed map[string]bool) {
	for i, r := range items {
		if r.Kind == ItemKindProfile {
			items[i].State = map[bool]string{true: ItemInstalled, false: ItemStaged}[installed[r.Identifier]]
		}
	}
}

// GuestPosture is the guest's posture as stage 2, or edit-base's recheck, read it from the base: not live,
// and never the Mac's own (RunnerPosture). A field is empty when its read did not say.
type GuestPosture struct {
	OSVersion string `json:"os_version,omitempty"`
	OSBuild   string `json:"os_build,omitempty"`
	// SIP and Gatekeeper are PostureOn or PostureOff.
	SIP        string `json:"sip,omitempty"`
	Gatekeeper string `json:"gatekeeper,omitempty"`
	// Profiles is how many configuration profiles are installed; nil when unread.
	Profiles *int `json:"profiles,omitempty"`
	// MDM is PostureEnrolled or PostureNotEnrolled.
	MDM string `json:"mdm,omitempty"`
	// OrgCA is ItemTrusted when every ca item reads trusted, else ItemNotTrusted; empty with no ca item.
	OrgCA string `json:"org_ca,omitempty"`
}

// GuestPosture values.
const (
	PostureOn          = "on"
	PostureOff         = "off"
	PostureEnrolled    = "enrolled"
	PostureNotEnrolled = "not_enrolled"
)

var (
	osVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}$`)
	osBuildRe   = regexp.MustCompile(`^[0-9A-Za-z]{1,16}$`)
)

// readPosture reads the guest's posture and the profile items' state. It is tolerant: a read that fails or
// prints what it does not know leaves its field empty, and never fails stage 2.
func readPosture(ctx context.Context, g ToolsGuest, items []ItemRecord) *GuestPosture {
	read := func(argv ...string) string {
		out, _, err := g.Run(ctx, argv)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(out)
	}
	match := func(out string, values map[string]string) string {
		for k, v := range values {
			if strings.Contains(out, k) {
				return v
			}
		}
		return ""
	}
	p := &GuestPosture{}
	if v := read("/usr/bin/sw_vers", "-productVersion"); osVersionRe.MatchString(v) {
		p.OSVersion = v
	}
	if v := read("/usr/bin/sw_vers", "-buildVersion"); osBuildRe.MatchString(v) {
		p.OSBuild = v
	}
	p.SIP = match(read("/usr/bin/csrutil", "status"), map[string]string{"status: enabled": PostureOn, "status: disabled": PostureOff})
	p.Gatekeeper = match(read("/usr/sbin/spctl", "--status"), map[string]string{"assessments enabled": PostureOn, "assessments disabled": PostureOff})
	p.MDM = match(read("/usr/bin/profiles", "status", "-type", "enrollment"), map[string]string{"MDM enrollment: Yes": PostureEnrolled, "MDM enrollment: No": PostureNotEnrolled})
	if installed, ok, _ := listProfiles(ctx, g); ok {
		n := len(installed)
		p.Profiles = &n
		readProfiles(items, installed)
	}
	for _, r := range items {
		if r.Kind == ItemKindCA {
			if p.OrgCA = r.State; r.State != ItemTrusted {
				break
			}
		}
	}
	return p
}

// StagedProfiles is the guest path of each profile item the base in dir holds staged and not yet
// installed: what macos-vm edit-base offers the person to approve.
func StagedProfiles(dir string) ([]string, error) {
	rec, err := ReadToolsRecord(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rec.Items {
		if r.Kind == ItemKindProfile && r.State == ItemStaged {
			out = append(out, guestagent.ItemDir(r.Name)+"/"+guestagent.ItemProfile)
		}
	}
	return out, nil
}

// restageProfiles stages again, as prepare does (0600 in its 0700 stage directory), each profile dir's
// tools.json holds as staged whose file the install list l still names with the same pin, and returns
// their guest paths: edit-base, before its window, since its last recheck removed them. A profile with no
// such file on this Mac is not staged and keeps its state.
func restageProfiles(ctx context.Context, g ToolsGuest, dir string, l BaseList) ([]string, error) {
	rec, err := ReadToolsRecord(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	files := map[string]BaseItem{}
	for _, it := range l.Items {
		if it.Kind == ItemKindProfile {
			files[it.Name] = it
		}
	}
	var out []string
	for _, r := range rec.Items {
		it, ok := files[r.Name]
		if r.Kind != ItemKindProfile || r.State != ItemStaged || !ok || it.SHA256 != r.SHA256 {
			continue
		}
		if _, err := os.Stat(it.Path); err != nil {
			continue
		}
		if _, err := installItem(ctx, g, it); err != nil {
			return nil, fmt.Errorf("macvm: base item %s (%s): %w", it.Name, it.Kind, err)
		}
		out = append(out, guestagent.ItemDir(it.Name)+"/"+guestagent.ItemProfile)
	}
	return out, nil
}

// removeStagedProfiles removes each staged profile (StagedProfiles' guest paths) with its stage directory,
// in edit-base's recheck: the first session in the guest after its window closes. A profile is installed
// by then, or still staged in the record until a later prepare or tools update stages it again: no tools
// session runs in the guest while the window is open to see an approval sooner.
func removeStagedProfiles(ctx context.Context, g ToolsGuest, staged []string) error {
	for _, p := range staged {
		if err := runItem(ctx, g, "/bin/rm", "-rf", filepath.Dir(p)); err != nil {
			return err
		}
	}
	return nil
}

// ReadBackItems reads the base items' state and the guest posture again in g, a boot of the base in dir,
// and records them in dir's tools.json: macos-vm edit-base's recheck calls it once the person has approved
// the staged profiles. A base without a tools.json has nothing to read back.
func ReadBackItems(ctx context.Context, g ToolsGuest, dir string) error {
	rec, err := ReadToolsRecord(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for i, r := range rec.Items {
		if r.Kind == ItemKindCA {
			if rec.Items[i].State, err = verifyCA(ctx, g, r.Name); err != nil {
				return err
			}
		}
	}
	rec.Guest = readPosture(ctx, g, rec.Items)
	return writeJSONAtomic(filepath.Join(dir, ToolsRecordFile), rec, 0o600)
}

func installItem(ctx context.Context, g ToolsGuest, it BaseItem) (rec ItemRecord, err error) {
	rec = ItemRecord{Kind: it.Kind, Name: it.Name, SHA256: it.SHA256, State: ItemInstalled, Identifier: it.Identifier}
	if it.Kind == ItemKindFile {
		if err := runItem(ctx, g, "/bin/mkdir", "-p", "-m", "0755", filepath.Dir(it.Dest)); err != nil {
			return rec, err
		}
		return rec, pushItem(ctx, g, it.Path, it.SHA256, it.Dest, 0o644)
	}
	dir := guestagent.ItemDir(it.Name)
	if err := runItem(ctx, g, "/bin/mkdir", "-p", "-m", "0700", dir); err != nil {
		return rec, err
	}
	switch it.Kind {
	case ItemKindProfile:
		// Kept staged for the person's approval in edit-base; readProfiles reads back whether it is installed.
		rec.State = ItemStaged
		return rec, pushItem(ctx, g, it.Path, it.SHA256, dir+"/"+guestagent.ItemProfile, 0o600)
	case ItemKindCA:
		return trustCA(ctx, g, it, rec)
	}
	// The stage directory goes after the install, whatever its end.
	defer func() {
		if rerr := runItem(ctx, g, "/bin/rm", "-rf", dir); err == nil && rerr != nil {
			err = fmt.Errorf("removing its stage directory: %w", rerr)
		}
	}()
	switch {
	case it.Kind == ItemKindPkg:
		pkg := dir + "/" + guestagent.ItemPkg
		if err := pushItem(ctx, g, it.Path, it.SHA256, pkg, 0o600); err != nil {
			return rec, err
		}
		for _, sf := range it.SecretFiles {
			if err := pushSecret(ctx, g, sf, dir+"/"+sf.Name); err != nil {
				return rec, fmt.Errorf("its secret file %s: %w", sf.Name, err)
			}
		}
		return rec, runItem(ctx, g, "/usr/sbin/installer", "-pkg", pkg, "-target", "/")
	case it.App == "":
		zip := dir + "/" + guestagent.ItemZip
		if err := pushItem(ctx, g, it.Path, it.SHA256, zip, 0o600); err != nil {
			return rec, err
		}
		return rec, runItem(ctx, g, "/usr/bin/ditto", "-x", "-k", zip, guestagent.MacApplications)
	}
	dmg, mnt := dir+"/"+guestagent.ItemDmg, dir+"/"+guestagent.ItemMount
	if err := pushItem(ctx, g, it.Path, it.SHA256, dmg, 0o600); err != nil {
		return rec, err
	}
	if err := runItem(ctx, g, "/usr/bin/hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", mnt, dmg); err != nil {
		return rec, err
	}
	err = runItem(ctx, g, "/usr/bin/ditto", mnt+"/"+it.App, guestagent.MacApplications+"/"+it.App)
	if derr := runItem(ctx, g, "/usr/bin/hdiutil", "detach", "-force", mnt); err == nil {
		err = derr
	}
	return rec, err
}

// trustCA stages a ca item's certificate (kept, for a trust by hand in edit-base), trusts it as a root
// in the System keychain for SSL only, never every policy, and then verifies it. The guest agent runs
// the trust inside the authorization bracket (guestagent withTrustSettingsRight), whose restore always
// runs. A trust Apple refuses without a person is not an error: the CA reads not_trusted.
func trustCA(ctx context.Context, g ToolsGuest, it BaseItem, rec ItemRecord) (ItemRecord, error) {
	b, err := os.ReadFile(it.Path)
	if err != nil {
		return rec, err
	}
	der := b
	if blk, _ := pem.Decode(b); blk != nil {
		der = blk.Bytes
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil || !cert.IsCA {
		return rec, fmt.Errorf("%s is not a CA certificate", it.Path)
	}
	s256, s1 := sha256.Sum256(der), sha1.Sum(der)
	rec.Fingerprint, rec.SHA1 = hex.EncodeToString(s256[:]), strings.ToUpper(hex.EncodeToString(s1[:]))
	cer := guestagent.ItemDir(it.Name) + "/" + guestagent.ItemCert
	if err := pushItem(ctx, g, it.Path, it.SHA256, cer, 0o600); err != nil {
		return rec, err
	}
	if _, _, err := g.Run(ctx, []string{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "ssl", "-k", guestagent.MacSystemKeychain, cer}); err != nil {
		return rec, err
	}
	rec.State, err = verifyCA(ctx, g, it.Name)
	return rec, err
}

// verifyCA is whether the guest trusts ca item name's staged certificate for SSL.
func verifyCA(ctx context.Context, g ToolsGuest, name string) (string, error) {
	_, exit, err := g.Run(ctx, []string{"/usr/bin/security", "verify-cert", "-L", "-p", "ssl", "-c", guestagent.ItemDir(name) + "/" + guestagent.ItemCert})
	if err != nil {
		return "", err
	}
	return map[bool]string{true: ItemTrusted, false: ItemNotTrusted}[exit == 0], nil
}

// removeRotatedCAs removes from the System keychain, with its trust settings, each CA the base held that
// the list no longer has (by fingerprint), and the stage directory of each item the list no longer names.
func removeRotatedCAs(ctx context.Context, g ToolsGuest, l BaseList, previous []ItemRecord) error {
	names, pins := map[string]bool{}, map[string]bool{}
	for _, it := range l.Items {
		names[it.Name], pins[it.SHA256] = true, true
	}
	for _, r := range previous {
		if r.Kind == ItemKindCA && r.SHA1 != "" && !pins[r.SHA256] {
			if err := runItem(ctx, g, "/usr/bin/security", "delete-certificate", "-t", "-Z", r.SHA1, guestagent.MacSystemKeychain); err != nil {
				return fmt.Errorf("macvm: removing the rotated CA %s (SHA-256 %s): %w", r.Name, r.Fingerprint, err)
			}
		}
		if !names[r.Name] && r.Kind != ItemKindFile {
			if err := runItem(ctx, g, "/bin/rm", "-rf", guestagent.ItemDir(r.Name)); err != nil {
				return fmt.Errorf("macvm: removing the stage directory of %s: %w", r.Name, err)
			}
		}
	}
	return nil
}

// pushItem checks the file at path against its pin and pushes it to dest through the same handle.
func pushItem(ctx context.Context, g ToolsGuest, path, pin, dest string, mode os.FileMode) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if ok, err := hasSHA256(f, pin); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%s does not match its pinned SHA-256", path)
	}
	return g.Push(ctx, dest, mode, f, fi.Size())
}

// pushSecret pushes a secret file, which must be its owner's alone (mode 0600 or narrower), as pushItem.
func pushSecret(ctx context.Context, g ToolsGuest, sf SecretFile, dest string) error {
	fi, err := os.Stat(sf.Path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is mode %#o; a secret file is its owner's alone (chmod 600)", sf.Path, fi.Mode().Perm())
	}
	return pushItem(ctx, g, sf.Path, sf.SHA256, dest, 0o600)
}

// runItem runs one item form and fails on a non-zero exit, naming the command and not its output.
func runItem(ctx context.Context, g ToolsGuest, argv ...string) error {
	_, exit, err := g.Run(ctx, argv)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("%s %s exited %d", filepath.Base(argv[0]), argv[1], exit)
	}
	return nil
}
