// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package main

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#cgo CFLAGS: -Wno-deprecated-declarations
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

// Every function takes and returns plain pointers, so the Go side never holds a Core Foundation type.

// wdc_identities returns the identities in the System keychain (a retained CFArray), or NULL: *st is 0 when
// there are none.
static const void *wdc_identities(int *st) {
	SecKeychainRef kc = NULL;
	OSStatus s = SecKeychainCopyDomainDefault(kSecPreferencesDomainSystem, &kc);
	if (s != errSecSuccess) { *st = (int)s; return NULL; }
	const void *kcs[] = { kc };
	CFArrayRef list = CFArrayCreate(NULL, kcs, 1, &kCFTypeArrayCallBacks);
	const void *keys[] = { kSecClass, kSecMatchLimit, kSecReturnRef, kSecMatchSearchList };
	const void *vals[] = { kSecClassIdentity, kSecMatchLimitAll, kCFBooleanTrue, list };
	CFDictionaryRef q = CFDictionaryCreate(NULL, keys, vals, 4, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFTypeRef res = NULL;
	s = SecItemCopyMatching(q, &res);
	CFRelease(q);
	CFRelease(list);
	CFRelease(kc);
	if (s == errSecItemNotFound) { *st = 0; return NULL; }
	*st = (int)s;
	if (s != errSecSuccess) return NULL;
	return res;
}

static long wdc_count(const void *arr) { return (long)CFArrayGetCount((CFArrayRef)arr); }

// wdc_identity_at returns the i-th identity, retained for the life of this one-shot process.
static const void *wdc_identity_at(const void *arr, long i) {
	const void *id = CFArrayGetValueAtIndex((CFArrayRef)arr, i);
	if (id == NULL || CFGetTypeID(id) != SecIdentityGetTypeID()) return NULL;
	return CFRetain(id);
}

// wdc_chain returns the identity's certificate chain, leaf first (a retained CFArray), or NULL. The chain is
// built whether or not the system trusts its root: the organisation judges trust itself.
static const void *wdc_chain(const void *id) {
	SecCertificateRef cert = NULL;
	if (SecIdentityCopyCertificate((SecIdentityRef)id, &cert) != errSecSuccess) return NULL;
	SecPolicyRef pol = SecPolicyCreateBasicX509();
	SecTrustRef trust = NULL;
	OSStatus s = SecTrustCreateWithCertificates(cert, pol, &trust);
	CFRelease(pol);
	if (s != errSecSuccess) { CFRelease(cert); return NULL; }
	CFErrorRef e = NULL;
	(void)SecTrustEvaluateWithError(trust, &e);
	if (e) CFRelease(e);
	CFArrayRef chain = SecTrustCopyCertificateChain(trust);
	CFRelease(trust);
	CFRelease(cert);
	return chain;
}

// wdc_der copies the i-th certificate of a chain as DER into a malloc'd buffer; *n is its length.
static unsigned char *wdc_der(const void *chain, long i, int *n) {
	SecCertificateRef c = (SecCertificateRef)CFArrayGetValueAtIndex((CFArrayRef)chain, i);
	CFDataRef d = SecCertificateCopyData(c);
	if (d == NULL) { *n = -1; return NULL; }
	*n = (int)CFDataGetLength(d);
	unsigned char *out = malloc((size_t)*n);
	if (out != NULL) memcpy(out, CFDataGetBytePtr(d), (size_t)*n);
	CFRelease(d);
	return out;
}

// wdc_sign signs msg with the identity's key: ECDSA (X9.62, over SHA-256) or RSA PKCS#1 v1.5 (over SHA-256).
// *n is the signature's length, -1 for a failure, -2 for a key of another type.
static unsigned char *wdc_sign(const void *id, const unsigned char *msg, int mlen, int *n) {
	SecKeyRef key = NULL;
	*n = -1;
	if (SecIdentityCopyPrivateKey((SecIdentityRef)id, &key) != errSecSuccess) return NULL;
	CFDictionaryRef attrs = SecKeyCopyAttributes(key);
	SecKeyAlgorithm alg = NULL;
	if (attrs != NULL) {
		CFTypeRef t = CFDictionaryGetValue(attrs, kSecAttrKeyType);
		if (t != NULL && CFEqual(t, kSecAttrKeyTypeECSECPrimeRandom)) alg = kSecKeyAlgorithmECDSASignatureMessageX962SHA256;
		else if (t != NULL && CFEqual(t, kSecAttrKeyTypeRSA)) alg = kSecKeyAlgorithmRSASignatureMessagePKCS1v15SHA256;
		CFRelease(attrs);
	}
	if (alg == NULL || !SecKeyIsAlgorithmSupported(key, kSecKeyOperationTypeSign, alg)) { CFRelease(key); *n = -2; return NULL; }
	CFDataRef data = CFDataCreate(NULL, msg, mlen);
	CFErrorRef e = NULL;
	CFDataRef sig = SecKeyCreateSignature(key, alg, data, &e);
	CFRelease(data);
	CFRelease(key);
	if (sig == NULL) { if (e) CFRelease(e); return NULL; }
	*n = (int)CFDataGetLength(sig);
	unsigned char *out = malloc((size_t)*n);
	if (out != NULL) memcpy(out, CFDataGetBytePtr(sig), (size_t)*n);
	CFRelease(sig);
	return out;
}

static void wdc_release(const void *p) { if (p != NULL) CFRelease(p); }
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/cjohnstoniv/wardyn/internal/devicecert"
)

// keychainIdentities reads the identities in the System keychain, where an MDM installs a device identity.
// Not yet tried on a real Mac: whether a root, ad hoc signed helper may use such an identity's key without a
// prompt is unconfirmed.
type keychainIdentities struct{}

func (keychainIdentities) Identities() ([]deviceIdentity, error) {
	var st C.int
	arr := C.wdc_identities(&st)
	if st != 0 {
		return nil, fmt.Errorf("the System keychain could not be searched (status %d)", int(st))
	}
	if arr == nil {
		return nil, nil
	}
	defer C.wdc_release(arr)
	var out []deviceIdentity
	n := int(C.wdc_count(arr))
	for i := 0; i < n; i++ {
		id := C.wdc_identity_at(arr, C.long(i))
		if id == nil {
			continue
		}
		chain, err := identityChain(id)
		if err != nil || len(chain) == 0 {
			continue
		}
		out = append(out, deviceIdentity{Chain: chain, sign: func(msg []byte) ([]byte, error) { return signWith(id, msg) }})
	}
	return out, nil
}

func identityChain(id unsafe.Pointer) ([][]byte, error) {
	chain := C.wdc_chain(id)
	if chain == nil {
		return nil, errors.New("no certificate chain")
	}
	defer C.wdc_release(chain)
	n := int(C.wdc_count(chain))
	if n > devicecert.MaxChainLen {
		n = devicecert.MaxChainLen
	}
	var out [][]byte
	for i := 0; i < n; i++ {
		var l C.int
		p := C.wdc_der(chain, C.long(i), &l)
		if p == nil || l <= 0 {
			return nil, errors.New("a certificate could not be read")
		}
		out = append(out, C.GoBytes(unsafe.Pointer(p), l))
		C.free(unsafe.Pointer(p))
	}
	return out, nil
}

func signWith(id unsafe.Pointer, msg []byte) ([]byte, error) {
	var l C.int
	var m *C.uchar
	if len(msg) > 0 {
		m = (*C.uchar)(unsafe.Pointer(&msg[0]))
	}
	p := C.wdc_sign(id, m, C.int(len(msg)), &l)
	if p == nil {
		if l == -2 {
			return nil, errKeyType
		}
		return nil, errors.New("the keychain refused to sign")
	}
	defer C.free(unsafe.Pointer(p))
	return C.GoBytes(unsafe.Pointer(p), l), nil
}
