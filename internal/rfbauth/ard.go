// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package rfbauth

import (
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"
)

// The reasons a DeclinedError gives, in the words the guest's desktop view status line uses.
const (
	ReasonNoAppleLogin = "no_apple_login"
	ReasonRefused      = "refused"
)

const (
	securityARD = 30
	ardTimeout  = 15 * time.Second
	// maxCredential is the longest user or password: each fills one 64-byte half of the credentials
	// block with its NUL.
	maxCredential = 63
	// maxKeyLen bounds the key length a server may announce (8192-bit), so a bad server cannot make
	// the client allocate or exponentiate without limit.
	maxKeyLen = 1024
	// maxName is the longest desktop name ServerInit may carry, the bound the helper's shim uses.
	maxName = 4 << 10
)

// ErrCredentialTooLong refuses a user or password longer than 63 bytes before any dial: Apple
// authentication's credentials block has no room for it, and a cut password would only be refused.
var ErrCredentialTooLong = errors.New("rfbauth: the user or the password is longer than 63 bytes")

// DeclinedError is Screen Sharing declining an Apple authentication sign-in: it offered no security
// type 30, or it refused the user and password. Neither changes during a boot, so an ARDClient
// keeps the first one.
type DeclinedError struct {
	Reason  string // ReasonNoAppleLogin or ReasonRefused
	Offered []byte // the security types the server offered
	Detail  string // the server's refusal reason, quoted, best effort
}

func (e *DeclinedError) Error() string {
	if e.Reason == ReasonNoAppleLogin {
		return fmt.Sprintf("rfbauth: Screen Sharing offers no Apple authentication (security types %v)", e.Offered)
	}
	return "rfbauth: Screen Sharing refused the sign-in: " + e.Detail
}

// ARDClient signs in to the Mac's Screen Sharing with Apple authentication (RFB security type 30).
// Hold one per boot of the guest: after the first DeclinedError, every later Open returns that same
// error and dials nothing, so watching the desktop again never repeats a refused sign-in against
// the account.
type ARDClient struct {
	// Dial opens a connection to Screen Sharing.
	Dial func(ctx context.Context) (net.Conn, error)

	mu       sync.Mutex
	declined *DeclinedError
}

// Open dials Screen Sharing and signs in as user, as an RFB 3.8 client sending a shared ClientInit,
// within 15 s or until ctx ends. It returns the connection, past ServerInit, and the ServerInit.
// Open keeps no copy of password; the caller clears it. Any error closes the connection. Opens run
// one at a time, so sessions that start together still sign in only once before a decline.
func (c *ARDClient) Open(ctx context.Context, user string, password []byte) (net.Conn, []byte, error) {
	if len(user) > maxCredential || len(password) > maxCredential {
		return nil, nil, ErrCredentialTooLong
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.declined != nil {
		return nil, nil, c.declined
	}
	ctx, cancel := context.WithTimeout(ctx, ardTimeout)
	defer cancel()
	conn, err := c.Dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Closing on ctx, not a deadline, also cuts the sign-in when the caller cancels (a view that
	// closed), so the next Open does not wait behind it.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	si, err := ardLogin(conn, user, password, rand.Reader)
	var d *DeclinedError
	declined := errors.As(err, &d)
	if !stop() && !declined {
		err = errors.Join(ctx.Err(), err) // the caller's cut, not Screen Sharing's answer
	}
	if err != nil {
		_ = conn.Close()
		if declined {
			c.declined = d
		}
		return nil, nil, err
	}
	return conn, si, nil
}

// ardLogin runs the RFB 3.8 handshake over conn with Apple authentication, through ServerInit.
func ardLogin(conn io.ReadWriter, user string, password []byte, random io.Reader) ([]byte, error) {
	offered, err := securityTypes(conn)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(offered, securityARD) {
		return nil, &DeclinedError{Reason: ReasonNoAppleLogin, Offered: offered}
	}
	if _, err := conn.Write([]byte{securityARD}); err != nil {
		return nil, err
	}
	msg, err := ardCredentials(conn, user, password, random)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}
	var result [4]byte
	if _, err := io.ReadFull(conn, result[:]); err != nil {
		return nil, fmt.Errorf("read the sign-in's result: %w", err)
	}
	if binary.BigEndian.Uint32(result[:]) != 0 {
		return nil, &DeclinedError{Reason: ReasonRefused, Offered: offered, Detail: reason(conn)}
	}
	if _, err := conn.Write([]byte{1}); err != nil { // ClientInit, shared
		return nil, err
	}
	return serverInit(conn)
}

// securityTypes reads the server's version (Apple's is 003.889), answers 3.8 and reads the security
// types the server offers.
func securityTypes(conn io.ReadWriter) ([]byte, error) {
	v := make([]byte, 12)
	if _, err := io.ReadFull(conn, v); err != nil {
		return nil, fmt.Errorf("read the server's version: %w", err)
	}
	minor, err := strconv.Atoi(string(v[8:11]))
	if string(v[:8]) != "RFB 003." || v[11] != '\n' || err != nil || minor < 8 {
		return nil, fmt.Errorf("the server speaks %q, not RFB 3.8 or later", v)
	}
	if _, err := io.WriteString(conn, "RFB 003.008\n"); err != nil {
		return nil, err
	}
	var n [1]byte
	if _, err := io.ReadFull(conn, n[:]); err != nil {
		return nil, fmt.Errorf("read the security types: %w", err)
	}
	if n[0] == 0 {
		return nil, fmt.Errorf("the server refused the connection: %s", reason(conn))
	}
	offered := make([]byte, n[0])
	if _, err := io.ReadFull(conn, offered); err != nil {
		return nil, fmt.Errorf("read the security types: %w", err)
	}
	return offered, nil
}

// ardCredentials reads the server's Diffie-Hellman exchange and returns the client's answer. The
// server sends g (u16), the key length L (u16), the prime p and its public key, L bytes each. The
// answer is a 128-byte credentials block, user and password each NUL-terminated in its own 64-byte
// half with random fill after, AES-128-ECB encrypted under MD5 of the shared key; then the client's
// public key. Both public keys and the shared key are left-padded to L bytes.
func ardCredentials(conn io.Reader, user string, password []byte, random io.Reader) ([]byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return nil, fmt.Errorf("read the key exchange: %w", err)
	}
	g := new(big.Int).SetUint64(uint64(binary.BigEndian.Uint16(head[:2])))
	n := int(binary.BigEndian.Uint16(head[2:]))
	if n == 0 || n > maxKeyLen {
		return nil, fmt.Errorf("the server's key length is %d bytes", n)
	}
	keys := make([]byte, 2*n)
	if _, err := io.ReadFull(conn, keys); err != nil {
		return nil, fmt.Errorf("read the key exchange: %w", err)
	}
	p := new(big.Int).SetBytes(keys[:n])
	if p.Cmp(big.NewInt(3)) <= 0 {
		return nil, errors.New("the server's prime is too small")
	}
	// The private key is n+8 random bytes reduced into [2, p-2]; the extra 64 bits keep the
	// reduction's bias negligible.
	pick := make([]byte, n+8)
	if _, err := io.ReadFull(random, pick); err != nil {
		return nil, err
	}
	x := new(big.Int).SetBytes(pick)
	clear(pick)
	x.Mod(x, new(big.Int).Sub(p, big.NewInt(3))).Add(x, big.NewInt(2))
	shared := new(big.Int).Exp(new(big.Int).SetBytes(keys[n:]), x, p).FillBytes(make([]byte, n))
	key := md5.Sum(shared)
	clear(shared)
	ci, err := aes.NewCipher(key[:])
	clear(key[:])
	if err != nil {
		return nil, err
	}
	block := make([]byte, 128)
	defer clear(block)
	if _, err := io.ReadFull(random, block); err != nil {
		return nil, err
	}
	copy(block, user)
	block[len(user)] = 0
	copy(block[64:], password)
	block[64+len(password)] = 0
	msg := make([]byte, 128, 128+n)
	for i := 0; i < 128; i += aes.BlockSize {
		ci.Encrypt(msg[i:i+aes.BlockSize], block[i:i+aes.BlockSize])
	}
	return append(msg, new(big.Int).Exp(g, x, p).FillBytes(make([]byte, n))...), nil
}

// reason reads a refusal's reason string, best effort, quoted for a log line.
func reason(r io.Reader) string {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return "(no reason)"
	}
	b := make([]byte, min(binary.BigEndian.Uint32(n[:]), maxName))
	k, _ := io.ReadFull(r, b)
	return strconv.Quote(string(b[:k]))
}

func serverInit(r io.Reader) ([]byte, error) {
	si := make([]byte, 24)
	if _, err := io.ReadFull(r, si); err != nil {
		return nil, fmt.Errorf("read ServerInit: %w", err)
	}
	name := binary.BigEndian.Uint32(si[20:])
	if name > maxName {
		return nil, fmt.Errorf("the server's desktop name is %d bytes", name)
	}
	si = append(si, make([]byte, name)...)
	if _, err := io.ReadFull(r, si[24:]); err != nil {
		return nil, fmt.Errorf("read ServerInit: %w", err)
	}
	return si, nil
}
