package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clawsynapse/pkg/types"
)

// apiTokenFile holds the local API bearer token. It is created with 0600 on
// first startup and reused afterwards, so a daemon restart does not
// invalidate tokens already distributed to callers (Phase 3.4 security debt).
const apiTokenFile = "api_token"

// LoadOrCreateAPIToken returns the local API bearer token, creating a fresh
// 32-byte random hex token on first use. The file is written with 0600 so
// only the daemon user can read it.
func LoadOrCreateAPIToken(dataDir string) (string, error) {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return "", fmt.Errorf("data dir is required for the local api token")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}

	path := filepath.Join(dataDir, apiTokenFile)
	if b, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(b)); token != "" {
			return token, nil
		}
		// Empty file: fall through and regenerate.
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read api token file: %w", err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate api token: %w", err)
	}
	token := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write api token file: %w", err)
	}
	return token, nil
}

// openPaths are served without a bearer token: the liveness probe and the
// peer trust handshake (a peer cannot know the token before pairing).
var openPaths = map[string]bool{
	"/v1/health":         true,
	"/v1/auth/challenge": true,
}

// requireBearer wraps mux with token auth. An empty token disables the
// check (legacy behavior for tests that construct a Server directly).
func requireBearer(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !openPaths[r.URL.Path] {
			auth := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(auth, prefix) || subtle.ConstantTimeCompare([]byte(auth[len(prefix):]), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="clawsynapse"`)
				respondJSON(w, http.StatusUnauthorized, types.APIResult{
					OK:      false,
					Code:    "unauthorized",
					Message: "missing or invalid bearer token",
					TS:      time.Now().UnixMilli(),
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
