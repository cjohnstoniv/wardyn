// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awskms

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/smithy-go/middleware"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// A Decrypt answer Unwrap refuses (another key's, or the wrong length) held a plaintext data key: the
// slice KMS handed back is zeroed before the error is returned.
func TestKEK_UnwrapZeroesARefusedPlaintext(t *testing.T) {
	f := newFakeKMS(t)
	k := newTestKEK(t, f)
	bind := kek.Bind("", "x")
	w, err := k.Wrap(t.Context(), testDEK(), bind)
	if err != nil {
		t.Fatal(err)
	}

	var answered []byte
	opts := k.client.Options()
	opts.APIOptions = append(opts.APIOptions, func(stack *middleware.Stack) error {
		return stack.Deserialize.Add(middleware.DeserializeMiddlewareFunc("capturePlaintext",
			func(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (middleware.DeserializeOutput, middleware.Metadata, error) {
				out, md, err := next.HandleDeserialize(ctx, in)
				if r, ok := out.Result.(*kms.DecryptOutput); ok {
					answered = r.Plaintext
				}
				return out, md, err
			}), middleware.Before)
	})
	k.client = kms.New(opts)

	for name, set := range map[string]func(){
		"another key's answer": func() { f.answerKeyID = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111" },
		"a short answer":       func() { f.answerKeyID, f.shortAnswer = "", true },
	} {
		f.mu.Lock()
		set()
		f.mu.Unlock()
		answered = nil
		if _, err := k.Unwrap(t.Context(), w, bind); err == nil {
			t.Fatalf("%s: Unwrap succeeded", name)
		}
		if len(answered) == 0 {
			t.Fatalf("%s: the capture saw no plaintext", name)
		}
		for i, b := range answered {
			if b != 0 {
				t.Fatalf("%s: byte %d of the refused plaintext is %#x, want it zeroed", name, i, b)
			}
		}
	}
}
