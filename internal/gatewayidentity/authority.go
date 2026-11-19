// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gatewayidentity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

const (
	// caLifetime is how long the client CA's own certificate is valid. Rotation,
	// not expiry, is how it is replaced: a long life keeps an expiry from being
	// an outage nobody planned.
	caLifetime = 10 * 365 * 24 * time.Hour
	// leafBackdate is how far before its signing a client certificate is valid,
	// for clock skew between wardynd and a gateway.
	leafBackdate = 30 * time.Second
)

// storedAuthority is the client CA's secret-store row. The previous CA is kept
// as its certificate alone: nothing signs with it again.
type storedAuthority struct {
	Key      string             `json:"key"`
	Cert     string             `json:"cert"`
	Previous *storedRetiredCert `json:"previous,omitempty"`
}

type storedRetiredCert struct {
	Cert     string    `json:"cert"`
	RetireAt time.Time `json:"retire_at"`
}

// Authority signs a run's gateway client certificate: an X.509 leaf whose one
// URI SAN is the run's SPIFFE ID, the same value as the token's act.sub. Its key
// is its own secret-store row, apart from the token's signing key: a gateway
// that trusts one trusts nothing the other signs, and rotating one leaves the
// other alone. Only wardynd holds it. Safe for concurrent use.
type Authority struct {
	key      *ecdsa.PrivateKey
	cert     *x509.Certificate
	previous *x509.Certificate
	retireAt time.Time
	now      func() time.Time
}

// NewAuthority builds an Authority from a stored row. An unreadable row is an
// error, never a reason to make a new CA: a person has to look at it.
func NewAuthority(raw []byte) (*Authority, error) {
	stored, key, cert, err := parseAuthority(raw)
	if err != nil {
		return nil, err
	}
	a := &Authority{key: key, cert: cert, now: time.Now}
	if stored.Previous != nil {
		if a.previous, err = parseCACert(stored.Previous.Cert); err != nil {
			return nil, err
		}
		if stored.Previous.RetireAt.IsZero() {
			return nil, errors.New("gateway client CA: the stored previous certificate has no retire time")
		}
		a.retireAt = stored.Previous.RetireAt
	}
	return a, nil
}

// RotateAuthority returns the row that follows raw. Nil raw makes the first CA.
// The current CA becomes the previous one, published until the last certificate
// it signed has expired plus retireOverlap; the older previous goes. With
// retirePrevious the replaced CA is not published at all, which is the recovery
// from a stolen key. A raw row that cannot be read is an error and makes no CA.
func RotateAuthority(raw []byte, retirePrevious bool, now time.Time) ([]byte, error) {
	var next storedAuthority
	if raw != nil {
		old, _, _, err := parseAuthority(raw)
		if err != nil {
			return nil, err
		}
		if !retirePrevious {
			next.Previous = &storedRetiredCert{Cert: old.Cert, RetireAt: now.Add(tokenLifetime + retireOverlap).UTC()}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("gateway client CA: generate key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Wardyn gateway client CA"},
		NotBefore:             now.Add(-leafBackdate),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("gateway client CA: sign the CA certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("gateway client CA: encode key: %w", err)
	}
	next.Key = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	next.Cert = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return json.Marshal(next)
}

// Issue signs a client certificate for pub naming spiffeID, and returns it as
// PEM with its expiry. It lives for lifetime, or for the longest a token may
// when lifetime is zero, negative or longer than that: a certificate never
// outlives the token it travels with. The leaf is an X509-SVID: one URI SAN,
// not a CA, no certificate or CRL signing.
func (a *Authority) Issue(pub *ecdsa.PublicKey, spiffeID string, lifetime time.Duration) ([]byte, time.Time, error) {
	id, err := spiffeid.FromString(spiffeID)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("gateway client CA: %w", err)
	}
	if pub == nil || pub.Curve != elliptic.P256() {
		return nil, time.Time{}, errors.New("gateway client CA: the key to certify is not P-256")
	}
	if lifetime <= 0 || lifetime > tokenLifetime {
		lifetime = tokenLifetime
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, time.Time{}, err
	}
	now := a.now()
	expires := now.Add(lifetime).UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		NotBefore:             now.Add(-leafBackdate),
		NotAfter:              expires,
		URIs:                  []*url.URL{id.URL()},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, pub, a.key)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("gateway client CA: sign: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), expires, nil
}

// BundlePEM is the published trust bundle: the current CA certificate and,
// until its retire time, the previous one. The clock is read on every call, so
// a CA retires whether or not wardynd restarts.
func (a *Authority) BundlePEM() []byte {
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.cert.Raw})
	if a.previous != nil && a.now().Before(a.retireAt) {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.previous.Raw})...)
	}
	return out
}

// Serial is the current CA certificate's serial number in hex.
func (a *Authority) Serial() string { return a.cert.SerialNumber.Text(16) }

// RetireAt is when the previous CA stops being published; zero with no previous CA.
func (a *Authority) RetireAt() time.Time { return a.retireAt }

func parseAuthority(raw []byte) (storedAuthority, *ecdsa.PrivateKey, *x509.Certificate, error) {
	var stored storedAuthority
	if err := json.Unmarshal(raw, &stored); err != nil {
		return storedAuthority{}, nil, nil, fmt.Errorf("gateway client CA: read the stored row: %w", err)
	}
	block, _ := pem.Decode([]byte(stored.Key))
	if block == nil {
		return storedAuthority{}, nil, nil, errors.New("gateway client CA: the stored row has no key")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return storedAuthority{}, nil, nil, fmt.Errorf("gateway client CA: parse the key: %w", err)
	}
	if key.Curve != elliptic.P256() {
		return storedAuthority{}, nil, nil, errors.New("gateway client CA: the key is not P-256")
	}
	cert, err := parseCACert(stored.Cert)
	if err != nil {
		return storedAuthority{}, nil, nil, err
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return storedAuthority{}, nil, nil, errors.New("gateway client CA: the stored key does not match the stored certificate")
	}
	return stored, key, cert, nil
}

func parseCACert(s string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("gateway client CA: a stored certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("gateway client CA: parse a stored certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("gateway client CA: a stored certificate is not a CA")
	}
	return cert, nil
}

// randomSerial is a 128-bit random serial number.
func randomSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("gateway client CA: serial: %w", err)
	}
	return n, nil
}
