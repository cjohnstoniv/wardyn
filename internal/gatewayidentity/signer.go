// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package gatewayidentity signs the short-lived token that tells an AI gateway
// which person, run and runner a model call belongs to, and holds the key that
// signs it. The key is separate from the run token's: publishing or rotating it
// never touches internal-API auth, and a token it signs cannot authenticate
// inward. Only wardynd holds it.
//
// The token format is "1": within it claims are only added, never removed,
// renamed or re-typed. See "Compatibility" in docs/operations/ai-gateway.md.
package gatewayidentity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
)

const (
	// tokenFormat is the wardyn_format claim: a gateway rule can tell which
	// shape it is reading. A change to this value is a new format, announced a
	// minor release ahead.
	tokenFormat = "1"
	// tokenLifetime is the longest a token lives, and how long one lives when the
	// provider sets none. A gateway cannot learn that a run was killed, so this is
	// also the longest a killed run's token can still be spent. Key retirement
	// reads it too, so a shorter token never outlives its key's publication.
	tokenLifetime = 10 * time.Minute
	// retireOverlap is what a replaced key stays published past the last token
	// it signed: enough for a gateway on the 60-second refetch the JWKS route
	// asks for to miss a few fetches, and for clock skew between it and wardynd.
	retireOverlap = 5 * time.Minute
)

// Claims is the token's body, and the one place its claim set is decided. The
// caller fills the identity fields from the run and the provider, never from a
// request; Sign fills the rest.
type Claims struct {
	Issuer   string `json:"iss"`                       // the trust domain, spelled as the run token spells it
	Audience string `json:"aud"`                       // the provider's gateway audience
	Subject  string `json:"sub"`                       // the person, or the service principal of a service run
	Run      string `json:"wardyn_run"`                // the run id
	Runner   string `json:"wardyn_runner"`             // the executor, else the runner target
	Provider string `json:"wardyn_provider,omitempty"` // the model provider id; empty on an MCP server's token
	MCP      string `json:"wardyn_mcp,omitempty"`      // the MCP server's key, on its token alone
	Sponsor  string `json:"sponsor,omitempty"`
	// Delegate is the id of the portal a delegated run was launched through; empty on a run its person
	// launched themselves. A flat claim, so a gateway rule needs no walk through act.
	Delegate string `json:"wardyn_delegate,omitempty"`

	ID        string   `json:"jti"`
	IssuedAt  int64    `json:"iat"`
	ExpiresAt int64    `json:"exp"`
	Format    string   `json:"wardyn_format"`
	Actor     actClaim `json:"act"`
}

// actClaim names the agent run acting for the subject, in the run token's shape. Act is the actor
// before it (RFC 8693 section 4.1: the outermost act is the current actor and the most deeply nested
// the least recent), so the Wardyn run sits outside the agent a portal named. A gateway that reads it
// should treat the nested actor as provenance: the RFC says prior actors are informational only.
type actClaim struct {
	Sub string    `json:"sub"`
	Iss string    `json:"iss,omitempty"`
	Act *actClaim `json:"act,omitempty"`
}

// NamePriorActor records the agent that asked for this run, as the actor before it. Sign keeps it and
// sets the run's own subject over it. An empty sub names no one.
func (c *Claims) NamePriorActor(sub, iss string) {
	if sub == "" {
		return
	}
	c.Actor.Act = &actClaim{Sub: sub, Iss: iss}
}

// storedKeys is the secret-store row. The previous key is kept as its public
// half: nothing signs with it again, so a read of the row yields one signing
// key at most.
type storedKeys struct {
	Current  string         `json:"current"`
	Previous *storedRetired `json:"previous,omitempty"`
}

type storedRetired struct {
	Public   string    `json:"public"`
	RetireAt time.Time `json:"retire_at"`
}

// Signer signs with the current key and publishes the current key and, until it
// retires, the previous one. Safe for concurrent use.
type Signer struct {
	key      *ecdsa.PrivateKey
	kid      string
	signer   jose.Signer
	previous *ecdsa.PublicKey
	retireAt time.Time
	now      func() time.Time
}

// New builds a Signer from a stored row. An unreadable row is an error, never a
// reason to make a new key: a person has to look at it.
func New(raw []byte) (*Signer, error) {
	stored, key, err := parseStored(raw)
	if err != nil {
		return nil, err
	}
	kid, err := keyID(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	if err != nil {
		return nil, fmt.Errorf("gateway identity: build signer: %w", err)
	}
	s := &Signer{key: key, kid: kid, signer: signer, now: time.Now}
	if stored.Previous != nil {
		if s.previous, err = parsePublic(stored.Previous.Public); err != nil {
			return nil, err
		}
		if stored.Previous.RetireAt.IsZero() {
			return nil, errors.New("gateway identity: the stored previous key has no retire time")
		}
		s.retireAt = stored.Previous.RetireAt
	}
	return s, nil
}

// Rotate returns the row that follows raw. Nil raw makes the first key. The
// current key becomes the previous one, published until its last token has
// expired plus retireOverlap; the older previous goes. With retirePrevious the
// replaced keys are not published at all, which is the recovery from a stolen
// key: the new key alone remains. A raw row that cannot be read, an empty one
// included, is an error and makes no key.
func Rotate(raw []byte, retirePrevious bool, now time.Time) ([]byte, error) {
	var next storedKeys
	if raw != nil {
		_, old, err := parseStored(raw)
		if err != nil {
			return nil, err
		}
		if !retirePrevious {
			public, err := marshalPublic(&old.PublicKey)
			if err != nil {
				return nil, err
			}
			next.Previous = &storedRetired{Public: public, RetireAt: now.Add(tokenLifetime + retireOverlap).UTC()}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("gateway identity: generate key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("gateway identity: encode key: %w", err)
	}
	next.Current = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	return json.Marshal(next)
}

// Sign signs c with the current key and returns the token and c as signed, whose
// ID and ExpiresAt the caller's audit row needs. A token missing an identity
// field is refused, so a caller's empty value never reaches a gateway as a
// signed one. The token lives for lifetime, or for the longest a token may when
// lifetime is zero, negative or longer than that.
func (s *Signer) Sign(c Claims, lifetime time.Duration) (string, Claims, error) {
	if c.Issuer == "" || c.Audience == "" || c.Subject == "" || c.Run == "" || (c.Provider == "") == (c.MCP == "") {
		return "", Claims{}, errors.New("gateway identity: issuer, audience, subject, run and exactly one of provider and MCP server are required")
	}
	if lifetime <= 0 || lifetime > tokenLifetime {
		lifetime = tokenLifetime
	}
	now := s.now()
	c.ID = uuid.NewString()
	c.IssuedAt = now.Unix()
	c.ExpiresAt = now.Add(lifetime).Unix()
	c.Format = tokenFormat
	c.Actor = actClaim{Sub: c.Issuer + "/agent-run/" + c.Run, Act: c.Actor.Act}
	token, err := jwt.Signed(s.signer).Claims(c).Serialize()
	if err != nil {
		return "", Claims{}, fmt.Errorf("gateway identity: sign: %w", err)
	}
	return token, c, nil
}

// JWKS returns the published key set: the current key and, until its retire
// time, the previous one. The clock is read on every call, so a key retires
// whether or not wardynd restarts.
func (s *Signer) JWKS() ([]byte, error) {
	keys := []jose.JSONWebKey{publicJWK(&s.key.PublicKey, s.kid)}
	if s.previous != nil && s.now().Before(s.retireAt) {
		kid, err := keyID(s.previous)
		if err != nil {
			return nil, err
		}
		keys = append(keys, publicJWK(s.previous, kid))
	}
	return json.Marshal(jose.JSONWebKeySet{Keys: keys})
}

// KeyID is the current key's kid.
func (s *Signer) KeyID() string { return s.kid }

// RetireAt is when the previous key stops being published; zero with no previous key.
func (s *Signer) RetireAt() time.Time { return s.retireAt }

func publicJWK(pub *ecdsa.PublicKey, kid string) jose.JSONWebKey {
	return jose.JSONWebKey{Key: pub, KeyID: kid, Algorithm: string(jose.ES256), Use: "sig"}
}

func parseStored(raw []byte) (storedKeys, *ecdsa.PrivateKey, error) {
	var stored storedKeys
	if err := json.Unmarshal(raw, &stored); err != nil {
		return storedKeys{}, nil, fmt.Errorf("gateway identity: read the stored row: %w", err)
	}
	block, _ := pem.Decode([]byte(stored.Current))
	if block == nil {
		return storedKeys{}, nil, errors.New("gateway identity: the stored row has no current key")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return storedKeys{}, nil, fmt.Errorf("gateway identity: parse the current key: %w", err)
	}
	if key.Curve != elliptic.P256() {
		return storedKeys{}, nil, errors.New("gateway identity: the current key is not P-256")
	}
	return stored, key, nil
}

func marshalPublic(pub *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("gateway identity: encode public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

func parsePublic(s string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("gateway identity: the stored previous key is not PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("gateway identity: parse the previous key: %w", err)
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("gateway identity: the stored previous key is not P-256")
	}
	return pub, nil
}

// keyID is the SHA-256 of the key's uncompressed point, the same derivation the
// run token's issuer uses for its kid.
func keyID(pub *ecdsa.PublicKey) (string, error) {
	point, err := pub.ECDH()
	if err != nil {
		return "", fmt.Errorf("gateway identity: key id: %w", err)
	}
	sum := sha256.Sum256(point.Bytes())
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}
