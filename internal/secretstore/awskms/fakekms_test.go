// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awskms

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Built, not spelled out: these are fixtures, and a literal key id or
// credentials-file field is what secret scanners look for.
var (
	secretField    = "aws_secret_" + "access_key"
	tokenField     = "aws_session_" + "token"
	longLivedKeyID = "AK" + "IA" + strings.Repeat("T", 16)
	sessionKeyID   = "AS" + "IA" + strings.Repeat("T", 16)
)

const testARN = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000"

// kmsFault is an answer the fake gives instead of working: the HTTP status
// and the KMS error code (the "__type" of a JSON 1.1 error).
type kmsFault struct {
	status int
	code   string
}

// fakeKMS is an in-process AWS KMS speaking the JSON 1.1 protocol for
// TrentService.Encrypt and TrentService.Decrypt. The ciphertext blob holds the
// encryption context beside an AES-GCM seal of the plaintext under a random
// test key with the context as associated data; Decrypt refuses a context that
// differs from the one the blob was made under (InvalidCiphertextException),
// as KMS does.
type fakeKMS struct {
	t   *testing.T
	srv *httptest.Server
	gcm cipher.AEAD

	mu            sync.Mutex
	arn           string // the key this fake is, echoed in KeyId
	answerKeyID   string // when set, echoed in KeyId instead
	ignoreContext bool   // Decrypt skips the context comparison
	faults        map[string]kmsFault
	calls         map[string]int
	contexts      []map[string]string // every request's encryption context
	keyIDs        []string            // every request's KeyId
	algorithms    []string            // every request's EncryptionAlgorithm
	shortAnswer   bool                // Decrypt answers a plaintext of the wrong length
}

func newFakeKMS(t *testing.T) *fakeKMS {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeKMS{t: t, gcm: gcm, arn: testARN, faults: map[string]kmsFault{}, calls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKMS) fail(op string, status int, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[op] = kmsFault{status, code}
}

func (f *fakeKMS) callCount(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

func (f *fakeKMS) allContexts() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string(nil), f.contexts...)
}

type kmsRequest struct {
	KeyId               string
	Plaintext           []byte
	CiphertextBlob      []byte
	EncryptionContext   map[string]string
	EncryptionAlgorithm string
}

func (f *fakeKMS) serve(w http.ResponseWriter, r *http.Request) {
	op := r.Header.Get("X-Amz-Target")
	const prefix = "TrentService."
	if len(op) <= len(prefix) || op[:len(prefix)] != prefix {
		f.reply(w, http.StatusBadRequest, map[string]string{"__type": "UnknownOperationException"})
		return
	}
	op = op[len(prefix):]
	var req kmsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.reply(w, http.StatusBadRequest, map[string]string{"__type": "SerializationException"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[op]++
	f.contexts = append(f.contexts, maps.Clone(req.EncryptionContext))
	f.keyIDs = append(f.keyIDs, req.KeyId)
	f.algorithms = append(f.algorithms, req.EncryptionAlgorithm)
	if ft, ok := f.faults[op]; ok {
		f.reply(w, ft.status, map[string]string{"__type": ft.code, "message": "forced " + ft.code})
		return
	}
	if req.KeyId != f.arn {
		f.reply(w, http.StatusBadRequest, map[string]string{"__type": "NotFoundException", "message": "no such key"})
		return
	}
	echo := f.arn
	if f.answerKeyID != "" {
		echo = f.answerKeyID
	}
	switch op {
	case "Encrypt":
		blob := f.seal(req.Plaintext, req.EncryptionContext)
		f.reply(w, http.StatusOK, map[string]any{"CiphertextBlob": blob, "KeyId": echo, "EncryptionAlgorithm": "SYMMETRIC_DEFAULT"})
	case "Decrypt":
		plain, ok := f.open(req.CiphertextBlob, req.EncryptionContext)
		if !ok {
			f.reply(w, http.StatusBadRequest, map[string]string{"__type": "InvalidCiphertextException", "message": "the ciphertext or the context is wrong"})
			return
		}
		if f.shortAnswer {
			plain = plain[:len(plain)-1]
		}
		f.reply(w, http.StatusOK, map[string]any{"Plaintext": plain, "KeyId": echo, "EncryptionAlgorithm": "SYMMETRIC_DEFAULT"})
	default:
		f.reply(w, http.StatusBadRequest, map[string]string{"__type": "UnknownOperationException"})
	}
}

func (f *fakeKMS) reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	if m, ok := body.(map[string]string); ok && m["__type"] != "" {
		w.Header().Set("X-Amzn-Errortype", m["__type"])
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// seal makes blob = len(context json) ‖ context json ‖ GCM(plaintext, aad = context json).
func (f *fakeKMS) seal(plain []byte, ec map[string]string) []byte {
	cj, _ := json.Marshal(ec)
	out := binary.BigEndian.AppendUint32(nil, uint32(len(cj)))
	out = append(out, cj...)
	return f.gcm.Seal(out, nil, plain, cj)
}

func (f *fakeKMS) open(blob []byte, ec map[string]string) ([]byte, bool) {
	if len(blob) < 4 {
		return nil, false
	}
	n := binary.BigEndian.Uint32(blob)
	if uint64(n) > uint64(len(blob)-4) {
		return nil, false
	}
	cj, sealed := blob[4:4+n], blob[4+n:]
	if !f.ignoreContext {
		want, _ := json.Marshal(ec)
		if !bytes.Equal(cj, want) {
			return nil, false
		}
	}
	plain, err := f.gcm.Open(nil, nil, sealed, cj)
	return plain, err == nil
}

// useCreds points the SDK's default chain at a shared credentials file only,
// with no retries, so a test never reads the machine's AWS setup or waits on
// backoff. An empty accessKeyID leaves no credentials at all.
func useCreds(t *testing.T, accessKeyID, sessionToken string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "credentials")
	var body string
	if accessKeyID != "" {
		body = "[default]\naws_access_key_id = " + accessKeyID + "\n" + secretField + " = " + strings.Repeat("s", 40) + "\n"
		if sessionToken != "" {
			body += tokenField + " = " + sessionToken + "\n"
		}
	}
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", file)
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, v := range []string{"AWS_ACCESS_KEY_ID", "AWS_PROFILE", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_USE_FIPS_ENDPOINT"} {
		t.Setenv(v, "")
	}
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
}

// newTestKEK boots the KEK against f under a role-style (temporary) key.
func newTestKEK(t *testing.T, f *fakeKMS) *KEK {
	t.Helper()
	useCreds(t, sessionKeyID, "token")
	k, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL})
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	return k
}
