/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one renewal string the eager shell draws: the expiry banner's button.
// Its canon row is REAUTH_RENEW.CTA (reauth-copy.ts takes it from here); it
// lives apart because reauth-copy.ts is lazy-side copy, and importing that
// module from the banner would pull every sentence in it into the entry chunk
// (bundle-split.test.ts).
export const SIGN_IN_AGAIN = "Sign in again";
