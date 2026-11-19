// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package machosign signs a thin 64-bit Mach-O executable with a certificate and key held in this process, so no
// keychain and no trust setting is involved. It writes what codesign writes for such a file: a SHA-256
// CodeDirectory, a designated requirement, and a detached CMS signature over the CodeDirectory, in an embedded
// signature that replaces the one the linker made. It is written from Apple's published formats: TN3126 and
// TN3127, xnu's cs_blobs.h, the Security framework's code signing sources (codedirectory, sigblob, requirement,
// reqmaker, reqdumper, signer), cctools' codesign_allocate, and the codesign(1) and csreq(1) manuals.
package machosign

import (
	"bytes"
	"crypto"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/digitorus/pkcs7"
)

// Mach-O constants (mach-o/loader.h).
const (
	mhMagic64       = 0xfeedfacf
	mhExecute       = 2
	lcSegment64     = 0x19
	lcCodeSignature = 0x1d
	headerSize      = 32
	segLinkeditPage = 0x4000 // __LINKEDIT's vmsize is rounded to a 16 KiB page
)

// Code signature constants (xnu cs_blobs.h; Security codedirectory.h, requirement.h, CSCommon.h).
const (
	magicEmbeddedSignature = 0xfade0cc0
	magicCodeDirectory     = 0xfade0c02
	magicRequirements      = 0xfade0c01
	magicRequirement       = 0xfade0c00
	magicBlobWrapper       = 0xfade0b01

	slotCodeDirectory = 0
	slotRequirements  = 2
	slotSignature     = 0x10000

	cdVersion       = 0x20400 // the first version with the exec segment fields
	cdHeaderSize    = 88      // through execSegFlags, for that version
	cdHashSHA256    = 2
	cdHashSize      = 32
	cdPageShift     = 12 // 4 KiB code pages
	cdSpecialSlots  = 2  // Info.plist (absent: zero) and the requirements
	execSegMainBin  = 0x1
	designatedType  = 3 // kSecDesignatedRequirementType
	exprForm        = 1
	opIdent         = 2
	opAnchorHash    = 4
	opAnd           = 6
	anchorCertSlot  = -1 // Requirement::anchorCert: the last certificate in the chain, "root" in requirement text
	cmsReserveExtra = 512
)

// Apple's CMS signed attributes for the CodeDirectory hashes (hash agility V1 and V2).
var (
	oidHashAgilityV1 = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 9, 1}
	oidHashAgilityV2 = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 9, 2}
)

// Requirements is the requirements set holding one designated requirement:
// `identifier "<identifier>" and certificate root = H"<rootSHA1>"`.
func Requirements(identifier string, rootSHA1 [20]byte) []byte {
	var expr []byte
	expr = be32(expr, opAnd)
	expr = be32(expr, opIdent)
	expr = data(expr, []byte(identifier))
	expr = be32(expr, opAnchorHash)
	slot := int32(anchorCertSlot)
	expr = be32(expr, uint32(slot)) // two's complement, as the reader takes an int32
	expr = data(expr, rootSHA1[:])
	req := be32(be32(be32(nil, magicRequirement), uint32(12+len(expr))), exprForm)
	req = append(req, expr...)
	const setHeader = 12 + 8 // magic, length, count, one index entry
	set := be32(be32(be32(nil, magicRequirements), uint32(setHeader+len(req))), 1)
	set = be32(be32(set, designatedType), setHeader)
	return append(set, req...)
}

// image is what Sign reads from the executable's load commands.
type image struct {
	codeSigCmd  int    // file offset of LC_CODE_SIGNATURE
	linkeditCmd int    // file offset of __LINKEDIT's segment command
	linkeditOff uint64 // __LINKEDIT's fileoff
	textOff     uint64 // __TEXT's fileoff and filesize: the exec segment
	textSize    uint64
	sigOff      uint32 // the linker's signature: offset and size
	sigSize     uint32
	main        bool // MH_EXECUTE
}

// Sign returns a copy of bin signed as identifier with cert and key. The linker's signature is replaced, never
// appended to: LC_CODE_SIGNATURE is reused at a 16-aligned offset with the new size, __LINKEDIT's filesize and
// vmsize follow it, and the old blob is cut off. The CodeDirectory carries no flags (not ad hoc, not
// linker-signed, no hardened runtime) and no team ID. Errors never name a path.
func Sign(bin []byte, identifier string, cert *x509.Certificate, key crypto.Signer) ([]byte, error) {
	img, err := parse(bin)
	if err != nil {
		return nil, err
	}
	dataoff := (uint64(img.sigOff) + 15) &^ 15
	if dataoff > 0xffffffff-0x100000 {
		return nil, errors.New("machosign: the executable is too large to sign")
	}
	reqs := Requirements(identifier, sha1.Sum(cert.Raw))
	nSlots := (dataoff + 1<<cdPageShift - 1) >> cdPageShift
	cdLen := cdHeaderSize + uint64(len(identifier)) + 1 + (cdSpecialSlots+nSlots)*cdHashSize
	// The CMS's size is fixed by the certificate, the key and the attributes, not by the CodeDirectory's
	// content: one made over a placeholder of the same length sizes the space, with room to spare.
	probe, err := signCMS(make([]byte, cdLen), cert, key)
	if err != nil {
		return nil, err
	}
	const superHeader = 12 + 3*8
	reserve := uint64(len(probe)) + cmsReserveExtra
	datasize := (superHeader + cdLen + uint64(len(reqs)) + 8 + reserve + 15) &^ 15

	out := make([]byte, dataoff+datasize)
	copy(out, bin[:img.sigOff])
	le := binary.LittleEndian
	le.PutUint32(out[img.codeSigCmd+8:], uint32(dataoff))
	le.PutUint32(out[img.codeSigCmd+12:], uint32(datasize))
	filesize := dataoff + datasize - img.linkeditOff
	le.PutUint64(out[img.linkeditCmd+32:], (filesize+segLinkeditPage-1)&^(segLinkeditPage-1)) // vmsize
	le.PutUint64(out[img.linkeditCmd+48:], filesize)

	cd := codeDirectory(out[:dataoff], identifier, reqs, img)
	if uint64(len(cd)) != cdLen {
		return nil, errors.New("machosign: the CodeDirectory's size changed while it was made")
	}
	cms, err := signCMS(cd, cert, key)
	if err != nil {
		return nil, err
	}
	if uint64(len(cms)) > reserve {
		return nil, fmt.Errorf("machosign: the CMS signature is %d bytes, more than the %d reserved", len(cms), reserve)
	}
	wrapper := append(be32(be32(nil, magicBlobWrapper), uint32(8+len(cms))), cms...)
	sb := be32(nil, magicEmbeddedSignature)
	sb = be32(sb, uint32(superHeader+len(cd)+len(reqs)+len(wrapper)))
	sb = be32(sb, 3)
	off := uint32(superHeader)
	for _, e := range []struct {
		slot uint32
		blob []byte
	}{{slotCodeDirectory, cd}, {slotRequirements, reqs}, {slotSignature, wrapper}} {
		sb = be32(be32(sb, e.slot), off)
		off += uint32(len(e.blob))
	}
	sb = append(append(append(sb, cd...), reqs...), wrapper...)
	copy(out[dataoff:], sb) // the rest of the reserved space stays zero
	return out, nil
}

// parse reads the load commands Sign needs and checks the linker's signature ends both __LINKEDIT and the file.
func parse(bin []byte) (image, error) {
	var img image
	le := binary.LittleEndian
	if len(bin) < headerSize || le.Uint32(bin) != mhMagic64 {
		return img, errors.New("machosign: not a thin 64-bit Mach-O executable")
	}
	img.main = le.Uint32(bin[12:]) == mhExecute
	ncmds, sizeofcmds := le.Uint32(bin[16:]), le.Uint32(bin[20:])
	if uint64(headerSize)+uint64(sizeofcmds) > uint64(len(bin)) {
		return img, errors.New("machosign: the load commands run past the end of the file")
	}
	cmds := bin[:headerSize+int(sizeofcmds)]
	var haveText, haveLinkedit, haveSig bool
	var linkeditEnd uint64
	at := headerSize
	for range ncmds {
		if at+8 > len(cmds) {
			return img, errors.New("machosign: a load command runs past the load commands")
		}
		cmd, size := le.Uint32(cmds[at:]), int(le.Uint32(cmds[at+4:]))
		if size < 8 || at+size > len(cmds) {
			return img, errors.New("machosign: a load command has a bad size")
		}
		switch cmd {
		case lcSegment64:
			if size < 72 {
				return img, errors.New("machosign: a segment command is too short")
			}
			name := string(bytes.TrimRight(cmds[at+8:at+24], "\x00"))
			fileoff, filesize := le.Uint64(cmds[at+40:]), le.Uint64(cmds[at+48:])
			switch name {
			case "__TEXT":
				img.textOff, img.textSize, haveText = fileoff, filesize, true
			case "__LINKEDIT":
				img.linkeditCmd, img.linkeditOff, linkeditEnd, haveLinkedit = at, fileoff, fileoff+filesize, true
			}
		case lcCodeSignature:
			if size != 16 {
				return img, errors.New("machosign: LC_CODE_SIGNATURE has a bad size")
			}
			img.codeSigCmd, img.sigOff, img.sigSize, haveSig = at, le.Uint32(cmds[at+8:]), le.Uint32(cmds[at+12:]), true
		}
		at += size
	}
	switch {
	case !haveText || !haveLinkedit:
		return img, errors.New("machosign: the executable has no __TEXT or no __LINKEDIT segment")
	case !haveSig:
		return img, errors.New("machosign: the executable has no LC_CODE_SIGNATURE to replace")
	case uint64(img.sigOff)+uint64(img.sigSize) != uint64(len(bin)) || linkeditEnd != uint64(len(bin)):
		return img, errors.New("machosign: the linker's signature does not end both __LINKEDIT and the file")
	case uint64(img.sigOff) < img.linkeditOff || uint64(img.sigOff) < uint64(len(cmds)):
		return img, errors.New("machosign: the linker's signature is not inside __LINKEDIT")
	}
	return img, nil
}

// codeDirectory is the CodeDirectory for code (the file up to the signature): the identifier, the special
// slots (requirements hashed, Info.plist zero), and a SHA-256 hash of each 4 KiB page, the last one short.
func codeDirectory(code []byte, identifier string, reqs []byte, img image) []byte {
	nSlots := (len(code) + 1<<cdPageShift - 1) >> cdPageShift
	identOff := cdHeaderSize
	hashOff := identOff + len(identifier) + 1 + cdSpecialSlots*cdHashSize
	length := hashOff + nSlots*cdHashSize
	cd := make([]byte, 0, length)
	cd = be32(cd, magicCodeDirectory)
	cd = be32(cd, uint32(length))
	cd = be32(cd, cdVersion)
	cd = be32(cd, 0) // flags
	cd = be32(cd, uint32(hashOff))
	cd = be32(cd, uint32(identOff))
	cd = be32(cd, cdSpecialSlots)
	cd = be32(cd, uint32(nSlots))
	cd = be32(cd, uint32(len(code))) // codeLimit
	cd = append(cd, cdHashSize, cdHashSHA256, 0 /* platform */, cdPageShift)
	cd = be32(cd, 0)                               // spare2
	cd = be32(cd, 0)                               // scatterOffset
	cd = be32(cd, 0)                               // teamOffset: no team ID
	cd = be32(cd, 0)                               // spare3
	cd = be64(cd, 0)                               // codeLimit64: codeLimit holds it
	cd = be64(be64(cd, img.textOff), img.textSize) // execSegBase, execSegLimit
	flags := uint64(0)
	if img.main {
		flags = execSegMainBin
	}
	cd = be64(cd, flags)
	cd = append(append(cd, identifier...), 0)
	// Special slots sit before hashOff in descending order: slot 2 (requirements), then slot 1 (Info.plist).
	reqSum := sha256.Sum256(reqs)
	cd = append(cd, reqSum[:]...)
	cd = append(cd, make([]byte, cdHashSize)...)
	for p := 0; p < len(code); p += 1 << cdPageShift {
		sum := sha256.Sum256(code[p:min(p+1<<cdPageShift, len(code))])
		cd = append(cd, sum[:]...)
	}
	return cd
}

// agileHash is one hash agility V2 value: the digest algorithm and the CodeDirectory's full hash.
type agileHash struct {
	Algorithm asn1.ObjectIdentifier
	Digest    []byte
}

// signCMS is the detached CMS signature over cd, SHA-256, with the signing certificate included and both hash
// agility attributes: V1, a plist of the CodeDirectory hashes cut to 20 bytes, and V2, each full hash with its
// algorithm.
func signCMS(cd []byte, cert *x509.Certificate, key crypto.Signer) ([]byte, error) {
	sum := sha256.Sum256(cd)
	v1 := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>cdhashes</key>
	<array>
		<data>` + base64.StdEncoding.EncodeToString(sum[:20]) + `</data>
	</array>
</dict>
</plist>
`)
	sd, err := pkcs7.NewSignedData(cd)
	if err != nil {
		return nil, fmt.Errorf("machosign: the CMS signature: %w", err)
	}
	sd.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256) // before AddSigner, which hashes with it
	if err := sd.AddSigner(cert, key, pkcs7.SignerInfoConfig{ExtraSignedAttributes: []pkcs7.Attribute{
		{Type: oidHashAgilityV1, Value: v1},
		{Type: oidHashAgilityV2, Value: agileHash{Algorithm: pkcs7.OIDDigestAlgorithmSHA256, Digest: sum[:]}},
	}}); err != nil {
		return nil, fmt.Errorf("machosign: the CMS signature: %w", err)
	}
	sd.Detach()
	b, err := sd.Finish()
	if err != nil {
		return nil, fmt.Errorf("machosign: the CMS signature: %w", err)
	}
	return b, nil
}

// be32 and be64 append v big-endian, as every code signature field is.
func be32(b []byte, v uint32) []byte { return binary.BigEndian.AppendUint32(b, v) }
func be64(b []byte, v uint64) []byte { return binary.BigEndian.AppendUint64(b, v) }

// data appends a requirement operand: its length, then its bytes padded with zeros to 4-byte alignment.
func data(b, v []byte) []byte {
	b = append(be32(b, uint32(len(v))), v...)
	return append(b, make([]byte, (4-len(v)%4)%4)...)
}
