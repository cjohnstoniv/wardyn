// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package machosign_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"debug/macho"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
	"time"

	gomacho "github.com/blacktop/go-macho"
	"github.com/digitorus/pkcs7"

	"github.com/cjohnstoniv/wardyn/internal/macvm/machosign"
	"github.com/cjohnstoniv/wardyn/internal/macvm/machosign/machosigntest"
)

const ident = "com.wardyn.guest-agent"

// identity is a self-signed code signing certificate and its key, as this Mac's agent signing identity is.
func identity(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "signer test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// The designated requirement `identifier "com.wardyn.guest-agent" and certificate root = H"<sha1>"`, byte for
// byte: a requirements set (0xfade0c01) holding one designated (type 3) requirement (0xfade0c00, expression
// form 1) of opAnd(6), opIdent(2) with its 4-byte-aligned string, and opAnchorHash(4) at the anchor slot. The
// slot is Apple's Requirement::anchorCert, -1 (requirement.h), which the decompiler (reqdumper.cpp certSlot)
// prints as "root"; leafCert, 0, would read "leaf".
func TestRequirementsBlobGolden(t *testing.T) {
	var root [20]byte
	for i := range root {
		root[i] = byte(i)
	}
	golden := "fade0c01" + "00000064" + "00000001" + "00000003" + "00000014" + // set: length 100, one entry, designated at 20
		"fade0c00" + "00000050" + "00000001" + // requirement: length 80, expression form
		"00000006" + // and
		"00000002" + "00000016" + hex.EncodeToString([]byte(ident)) + "0000" + // identifier, 22 bytes padded to 24
		"00000004" + "ffffffff" + "00000014" + "000102030405060708090a0b0c0d0e0f10111213" // certificate root = H"..."
	if got := hex.EncodeToString(machosign.Requirements(ident, root)); got != golden {
		t.Fatalf("requirements blob\n got %s\nwant %s", got, golden)
	}
}

// signed is a signed copy's parts as the tests read them.
type signed struct {
	out             []byte
	dataoff, datasz uint32
	cd, reqs, cms   []byte
	linkedit        *macho.Segment
	slotTypes       []uint32
	superblobLen    uint32
	textSize        int
}

func readSigned(t *testing.T, out []byte) signed {
	t.Helper()
	f, err := macho.NewFile(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("the signed copy does not parse as Mach-O: %v", err)
	}
	s := signed{out: out}
	for _, l := range f.Loads {
		raw := l.Raw()
		if binary.LittleEndian.Uint32(raw) == 0x1d {
			s.dataoff, s.datasz = binary.LittleEndian.Uint32(raw[8:]), binary.LittleEndian.Uint32(raw[12:])
		}
	}
	s.linkedit = f.Segment("__LINKEDIT")
	if s.dataoff == 0 || s.linkedit == nil || f.Segment("__TEXT") == nil {
		t.Fatal("no LC_CODE_SIGNATURE, __TEXT or __LINKEDIT")
	}
	s.textSize = int(f.Segment("__TEXT").Filesz)
	sig := out[s.dataoff : s.dataoff+s.datasz]
	be := binary.BigEndian
	if be.Uint32(sig) != 0xfade0cc0 {
		t.Fatalf("SuperBlob magic %#x", be.Uint32(sig))
	}
	s.superblobLen = be.Uint32(sig[4:])
	for i := range be.Uint32(sig[8:]) {
		typ, off := be.Uint32(sig[12+8*i:]), be.Uint32(sig[16+8*i:])
		s.slotTypes = append(s.slotTypes, typ)
		blob := sig[off : off+be.Uint32(sig[off+4:])]
		switch typ {
		case 0:
			s.cd = blob
		case 2:
			s.reqs = blob
		case 0x10000:
			if be.Uint32(blob) != 0xfade0b01 {
				t.Fatalf("CMS wrapper magic %#x", be.Uint32(blob))
			}
			s.cms = blob[8:]
		}
	}
	return s
}

// A signed copy replaces the linker's ad hoc signature: LC_CODE_SIGNATURE is reused at a 16-aligned offset, the
// old blob is gone, __LINKEDIT ends at the new signature's end with its vmsize rounded to 16 KiB, and the code
// before the signature is unchanged. go-macho re-parses it: one SHA-256 CodeDirectory, version 0x20400, flags 0
// (no adhoc, no linker-signed, no hardened runtime), no team ID, the identifier, both special slots, every page
// hash, the exec segment, and the designated requirement; and the CMS verifies over the CodeDirectory with both
// hash agility attributes.
func TestSignedCopyReparses(t *testing.T) {
	cert, key := identity(t)
	for _, linkedit := range []int{0x230, 0x123} {
		in := machosigntest.MachO(3, linkedit)
		oldOff := 3*0x4000 + linkedit
		out, err := machosign.Sign(in, ident, cert, key)
		if err != nil {
			t.Fatalf("linkedit %#x: %v", linkedit, err)
		}
		s := readSigned(t, out)
		wantOff := (oldOff + 15) &^ 15
		if int(s.dataoff) != wantOff || s.dataoff%16 != 0 || int(s.dataoff+s.datasz) != len(out) {
			t.Fatalf("dataoff %#x datasize %#x file %#x, want dataoff %#x ending the file", s.dataoff, s.datasz, len(out), wantOff)
		}
		if s.linkedit.Offset+s.linkedit.Filesz != uint64(len(out)) || s.linkedit.Memsz != (s.linkedit.Filesz+0x3fff)&^0x3fff {
			t.Fatalf("__LINKEDIT fileoff %#x filesize %#x vmsize %#x, file %#x", s.linkedit.Offset, s.linkedit.Filesz, s.linkedit.Memsz, len(out))
		}
		if bytes.Contains(out, []byte(machosigntest.OldSignatureMark)) {
			t.Fatal("the old signature blob is still in the copy")
		}
		if !bytes.Equal(out[0x1000:oldOff], in[0x1000:oldOff]) || !bytes.Equal(in[:0x1000], machosigntest.MachO(3, linkedit)[:0x1000]) {
			t.Fatal("the code before the signature changed")
		}
		if want := []uint32{0, 2, 0x10000}; !equalU32(s.slotTypes, want) {
			t.Fatalf("SuperBlob slots %#x, want %#x", s.slotTypes, want)
		}
		if int(s.superblobLen) > int(s.datasz) || !allZero(out[int(s.dataoff)+int(s.superblobLen):]) {
			t.Fatal("the reserved space after the SuperBlob is not zero padding")
		}

		gm, err := gomacho.NewFile(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("go-macho: %v", err)
		}
		cs := gm.CodeSignature()
		if cs == nil || len(cs.Errors) != 0 || len(cs.CodeDirectories) != 1 {
			t.Fatalf("go-macho code signature %+v", cs)
		}
		cd := cs.CodeDirectories[0]
		h := cd.Header
		if cd.ID != ident || cd.TeamID != "" || uint32(h.Version) != 0x20400 || uint32(h.Flags) != 0 || h.HashSize != 32 || uint8(h.HashType) != 2 ||
			h.PageSize != 12 || uint8(h.Platform) != 0 || h.NSpecialSlots != 2 || h.CodeLimit != s.dataoff || h.ScatterOffset != 0 {
			t.Fatalf("CodeDirectory %q team %q header %+v", cd.ID, cd.TeamID, h)
		}
		if h.ExecSegBase != 0 || h.ExecSegLimit != uint64(s.textSize) || uint64(h.ExecSegFlags) != 1 {
			t.Fatalf("exec segment base %#x limit %#x flags %#x, want 0, __TEXT's %#x, main binary", h.ExecSegBase, h.ExecSegLimit, h.ExecSegFlags, s.textSize)
		}
		pages := (int(s.dataoff) + 4095) / 4096
		if len(cd.CodeSlots) != pages {
			t.Fatalf("%d code slots, want %d", len(cd.CodeSlots), pages)
		}
		for i, slot := range cd.CodeSlots {
			sum := sha256.Sum256(out[i*4096 : min((i+1)*4096, int(s.dataoff))])
			if !bytes.Equal(slot.Hash, sum[:]) {
				t.Fatalf("page %d hash does not verify", i)
			}
		}
		rootSHA1 := sha1.Sum(cert.Raw)
		reqSum := sha256.Sum256(machosign.Requirements(ident, rootSHA1))
		for _, sp := range cd.SpecialSlots {
			want := make([]byte, 32)
			if sp.Index == 2 {
				want = reqSum[:]
			}
			if !bytes.Equal(sp.Hash, want) {
				t.Fatalf("special slot %d hash %x, want %x", sp.Index, sp.Hash, want)
			}
		}
		if !bytes.Equal(s.reqs, machosign.Requirements(ident, rootSHA1)) {
			t.Fatal("the requirements blob in the signature is not the designated requirement")
		}
		wantDR := `identifier "` + ident + `" and certificate root = H"` + hex.EncodeToString(rootSHA1[:]) + `"`
		if len(cs.Requirements) != 1 || machosigntest.AppleText(cs.Requirements[0].Detail) != wantDR || uint32(cs.Requirements[0].Type) != 3 {
			t.Fatalf("requirements %+v, want designated %q", cs.Requirements, wantDR)
		}

		p7, err := pkcs7.Parse(cs.CMSSignature)
		if err != nil {
			t.Fatal(err)
		}
		if len(p7.Content) != 0 || len(p7.Certificates) != 1 || !p7.Certificates[0].Equal(cert) || len(p7.Signers) != 1 ||
			!p7.Signers[0].DigestAlgorithm.Algorithm.Equal(pkcs7.OIDDigestAlgorithmSHA256) {
			t.Fatalf("CMS: content %d bytes, %d certificates, %d signers", len(p7.Content), len(p7.Certificates), len(p7.Signers))
		}
		p7.Content = s.cd
		pool := x509.NewCertPool()
		pool.AddCert(cert)
		if err := p7.VerifyWithOpts(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}); err != nil {
			t.Fatalf("the CMS does not verify over the CodeDirectory: %v", err)
		}
		cdSum := sha256.Sum256(s.cd)
		var v2 struct {
			Alg    asn1.ObjectIdentifier
			Digest []byte
		}
		if err := p7.UnmarshalSignedAttribute(asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 9, 2}, &v2); err != nil ||
			!v2.Alg.Equal(pkcs7.OIDDigestAlgorithmSHA256) || !bytes.Equal(v2.Digest, cdSum[:]) {
			t.Fatalf("hash agility V2 %+v: %v", v2, err)
		}
		var v1 []byte
		if err := p7.UnmarshalSignedAttribute(asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 9, 1}, &v1); err != nil ||
			!bytes.Contains(v1, []byte("<key>cdhashes</key>")) || !bytes.Contains(v1, []byte(base64.StdEncoding.EncodeToString(cdSum[:20]))) {
			t.Fatalf("hash agility V1 %q: %v", v1, err)
		}
	}
}

// Anything but a thin 64-bit Mach-O whose code signature ends both __LINKEDIT and the file is refused, with no
// path in the error.
func TestSignRefuses(t *testing.T) {
	cert, key := identity(t)
	good := machosigntest.MachO(1, 0x40)
	noSig := bytes.Clone(good)
	binary.LittleEndian.PutUint32(noSig[32+72+152+72:], 0x2a) // LC_CODE_SIGNATURE renamed LC_SOURCE_VERSION
	fat := bytes.Clone(good)
	binary.BigEndian.PutUint32(fat, 0xcafebabe)
	for name, in := range map[string][]byte{
		"not Mach-O":          []byte("new agent"),
		"fat":                 fat,
		"32-bit":              append([]byte{0xce, 0xfa, 0xed, 0xfe}, good[4:]...),
		"no code signature":   noSig,
		"bytes after the sig": append(bytes.Clone(good), 0, 0, 0, 0),
		"truncated":           good[:len(good)-8],
	} {
		if _, err := machosign.Sign(in, ident, cert, key); err == nil || strings.Contains(err.Error(), "/") {
			t.Fatalf("%s: err %v, want a refusal with no path", name, err)
		}
	}
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
