// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"net/http"
	"testing"
)

// The duplicate-key check folds keys the way json.Unmarshal binds them, so
// "duplicate" means "lands in the same field". U+017F (long s) folds with "s"
// in the decoder but not under strings.ToLower; U+212A (Kelvin sign) folds
// with "k" under both.
func TestDecodeUniqueFoldsLikeTheDecoder(t *testing.T) {
	pr := "/acme/proj/_apis/git/repositories/r1/pullrequests/7"
	runCases(t, []caseT{
		{name: "long s after the ASCII key", req: adoReq(http.MethodPatch, pr, `{"completionOptions":{"bypassPolicy":false,"bypaſsPolicy":true}}`), wantErr: true},
		{name: "long s before the ASCII key", req: adoReq(http.MethodPatch, pr, `{"completionOptions":{"bypaſsPolicy":true,"bypassPolicy":false}}`), wantErr: true},
		{name: "long s as a JSON escape", req: adoReq(http.MethodPatch, pr, `{"completionOptions":{"bypassPolicy":false,"bypa\u017fsPolicy":true}}`), wantErr: true},
		{name: "long s in a nested array object", req: adoReq(http.MethodPatch, pr, `{"completionOptions":{"x":[{"status":1,"ſtatus":2}]}}`), wantErr: true},
		{name: "an ordinary case pair", req: adoReq(http.MethodPatch, pr, `{"Status":"active","status":"completed"}`), wantErr: true},
		{name: "a Kelvin sign pair", req: adoReq(http.MethodPatch, pr, `{"\u212aey":1,"key":2}`), wantErr: true},
		{
			name: "a lone long-s key is read as the decoder reads it",
			req:  adoReq(http.MethodPatch, pr, `{"completionOptions":{"bypaſsPolicy":true}}`),
			want: CapPolicyBypass,
		},
		{
			name: "unrelated Unicode keys are not duplicates",
			req:  adoReq(http.MethodPatch, pr, `{"status":"completed","名前":1,"ключ":2,"КЛЮЧ2":3,"straße":4,"strasse":5,"ſ":6}`),
			want: CapPR,
		},
	})
}
