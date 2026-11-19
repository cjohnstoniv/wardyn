// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package machosigntest makes small Mach-O executables for the signer's tests.
package machosigntest

import (
	"encoding/binary"
	"encoding/hex"
	"strings"
)

// OldSignatureMark is in the fixture's ad hoc signature only: a signed copy that still holds it kept the old blob.
const OldSignatureMark = "the linker's ad hoc signature"

// MachO is a minimal 64-bit arm64 executable laid out the way the Go linker lays one out: __PAGEZERO, __TEXT of
// textPages 16 KiB pages with one section, __LINKEDIT of linkedit bytes followed by an ad hoc code signature, and
// LC_CODE_SIGNATURE naming that signature, which ends both __LINKEDIT and the file. A linkedit that is not a
// multiple of 16 leaves the signature's offset unaligned.
func MachO(textPages, linkedit int) []byte {
	const (
		page     = 0x4000
		vmBase   = uint64(1) << 32
		cmdsSize = 72 + 72 + 80 + 72 + 16
		oldSig   = 0x100
	)
	textSize := textPages * page
	sigOff := textSize + linkedit
	b := make([]byte, sigOff+oldSig)
	for i := 0x1000; i < sigOff; i++ {
		b[i] = byte(i*31 + 7)
	}
	le := binary.LittleEndian
	at := 0
	u32 := func(v uint32) { le.PutUint32(b[at:], v); at += 4 }
	u64 := func(v uint64) { le.PutUint64(b[at:], v); at += 8 }
	name := func(s string) { copy(b[at:at+16], s); at += 16 }
	segment := func(seg string, size uint32, vmaddr, vmsize, fileoff, filesize uint64, prot uint32, nsects uint32) {
		u32(0x19) // LC_SEGMENT_64
		u32(size)
		name(seg)
		u64(vmaddr)
		u64(vmsize)
		u64(fileoff)
		u64(filesize)
		u32(prot)
		u32(prot)
		u32(nsects)
		u32(0)
	}
	// mach_header_64: MH_MAGIC_64, CPU_TYPE_ARM64, MH_EXECUTE, 4 commands, NOUNDEFS|DYLDLINK|TWOLEVEL|PIE.
	u32(0xfeedfacf)
	u32(0x0100000c)
	u32(0)
	u32(2)
	u32(4)
	u32(cmdsSize)
	u32(0x00200085)
	u32(0)
	segment("__PAGEZERO", 72, 0, vmBase, 0, 0, 0, 0)
	segment("__TEXT", 72+80, vmBase, uint64(textSize), 0, uint64(textSize), 5, 1)
	name("__text")
	name("__TEXT")
	u64(vmBase + 0x1000)
	u64(uint64(textSize - 0x1000))
	u32(0x1000)
	u32(2)
	u32(0)
	u32(0)
	u32(0x80000400)
	u32(0)
	u32(0)
	u32(0)
	segment("__LINKEDIT", 72, vmBase+uint64(textSize), uint64(linkedit+oldSig), uint64(textSize), uint64(linkedit+oldSig), 1, 0)
	u32(0x1d) // LC_CODE_SIGNATURE
	u32(16)
	u32(uint32(sigOff))
	u32(oldSig)
	// An empty embedded signature SuperBlob, then the mark.
	be := binary.BigEndian
	be.PutUint32(b[sigOff:], 0xfade0cc0)
	be.PutUint32(b[sigOff+4:], 12)
	copy(b[sigOff+12:], strings.Repeat(OldSignatureMark, 2))
	return b
}

// AppleText is a requirement go-macho decompiled, as Apple's decompiler (reqdumper) prints it: go-macho prints a
// certificate hash's raw bytes where reqdumper prints H"<hex>".
func AppleText(detail string) string {
	pre, hash, ok := strings.Cut(detail, "certificate root = ")
	if !ok {
		return detail
	}
	return pre + `certificate root = H"` + hex.EncodeToString([]byte(hash)) + `"`
}
