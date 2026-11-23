// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/url"
	"sort"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The bounds on the MDM device CA settings.
const (
	mdmCAPEMMaxBytes = 64 << 10
	mdmCAMaxCerts    = 16
	mdmCRLURLMaxLen  = 2048
)

// parseMDMBundle reads the configured MDM CA bundle: one to mdmCAMaxCerts CERTIFICATE blocks and nothing
// else, each a CA certificate with valid basic constraints.
func parseMDMBundle(bundle string) ([]*x509.Certificate, string) {
	if len(bundle) > mdmCAPEMMaxBytes {
		return nil, "mdm_device_ca_pem is larger than 64 KiB"
	}
	var certs []*x509.Certificate
	rest := []byte(bundle)
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			return nil, "mdm_device_ca_pem may hold CERTIFICATE blocks only"
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, "mdm_device_ca_pem holds a certificate that cannot be read"
		}
		if !c.BasicConstraintsValid || !c.IsCA {
			return nil, "mdm_device_ca_pem may hold CA certificates only"
		}
		certs = append(certs, c)
	}
	if strings.TrimSpace(string(rest)) != "" {
		return nil, "mdm_device_ca_pem holds data that is not a PEM certificate"
	}
	if len(certs) == 0 {
		return nil, "mdm_device_ca_pem holds no certificate"
	}
	if len(certs) > mdmCAMaxCerts {
		return nil, "mdm_device_ca_pem holds more than 16 certificates"
	}
	return certs, ""
}

// validateMDMDeviceSettings is "" when the MDM device CA and CRL URL in st are usable: both empty, or
// both set to a bundle of CA certificates and an absolute http(s) URL.
func validateMDMDeviceSettings(st types.RunnerSettings) string {
	if st.MDMDeviceCAPEM == "" && st.MDMDeviceCRLURL == "" {
		return ""
	}
	if st.MDMDeviceCAPEM == "" || st.MDMDeviceCRLURL == "" {
		return "set mdm_device_ca_pem and mdm_device_crl_url together, or clear both"
	}
	if _, msg := parseMDMBundle(st.MDMDeviceCAPEM); msg != "" {
		return msg
	}
	if len(st.MDMDeviceCRLURL) > mdmCRLURLMaxLen {
		return "mdm_device_crl_url is longer than 2048 bytes"
	}
	u, err := url.Parse(st.MDMDeviceCRLURL)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "mdm_device_crl_url must be an absolute http or https URL with a host and no user info"
	}
	return ""
}

// mdmBundleFingerprints is the sorted SHA-256 hex of each certificate in a bundle, what the audit row
// records instead of the PEM. An unreadable bundle gives none.
func mdmBundleFingerprints(bundle string) []string {
	out := []string{}
	rest := []byte(bundle)
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		sum := sha256.Sum256(blk.Bytes)
		out = append(out, hex.EncodeToString(sum[:]))
	}
	sort.Strings(out)
	return out
}
