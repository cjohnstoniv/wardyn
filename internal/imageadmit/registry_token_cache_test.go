// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// One client serves the daemon's life and caches a Bearer token per registry and repository scope, so the
// cache must not grow with every distinct repository name checked: an expired entry goes on the next write,
// and a write at CacheEntries live entries replaces one instead of adding to them.
func TestTokenCacheStaysBounded(t *testing.T) {
	for _, c := range []struct {
		name    string
		expires time.Duration
		want    int
	}{
		{name: "expired entries are dropped on the next write", expires: -time.Minute, want: 1},
		{name: "a full cache replaces an entry instead of growing", expires: time.Minute, want: CacheEntries},
	} {
		t.Run(c.name, func(t *testing.T) {
			var fetched atomic.Int32
			realm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fetched.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok", "expires_in": 300})
			}))
			defer realm.Close()
			reg := bearerRegistry("https://127.0.0.1:"+port(realm)+"/token", "tok", nil)
			defer reg.Close()
			client := newTestClient(t, nil, testGuard(), nil, reg, realm)
			for i := range CacheEntries {
				client.tokens[fmt.Sprintf("%s|repository:team/app-%d:pull", host(reg), i)] = cachedToken{header: "Bearer old", expires: time.Now().Add(c.expires)}
			}

			for range 2 {
				if _, err := client.Manifest(context.Background(), mustRef(t, host(reg)+"/team/app:1.0")); err != nil {
					t.Fatal(err)
				}
			}
			client.mu.Lock()
			n := len(client.tokens)
			client.mu.Unlock()
			if n != c.want {
				t.Errorf("the cache holds %d tokens after one more scope, want %d", n, c.want)
			}
			if got := fetched.Load(); got != 1 {
				t.Errorf("the realm was asked %d times for one scope, want once: the new token was not kept", got)
			}
		})
	}
}
