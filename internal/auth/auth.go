// Package auth works out the bearer token Flixer must present.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
)

// label separates this token from every other use of the shared secret. Flixer derives the token the same way.
const label = "flixer-updater-token"

// Derive returns the token for a shared secret: hex(HMAC-SHA256(secret, label)).
func Derive(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(label))
	return hex.EncodeToString(mac.Sum(nil))
}

// Resolver returns the expected token. An explicit token wins; otherwise it is derived from the secret file, which Flixer
// may only write after the updater has started, so it is read on every call. An empty result rejects every request.
func Resolver(explicit, secretFile string) func() string {
	return func() string {
		if explicit != "" {
			return explicit
		}
		raw, err := os.ReadFile(secretFile)
		if err != nil || len(raw) == 0 {
			return ""
		}
		return Derive(string(raw))
	}
}
