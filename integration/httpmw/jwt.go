package httpmw

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// unsafeParseJWTClaims decodes the payload section of a JWT without verifying
// the signature. This is intentional: the app's own auth middleware must
// validate the token; we only read claims for audit attribution.
func unsafeParseJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("httpmw: malformed JWT (expected 3 parts, got %d)", len(parts))
	}

	payload := parts[1]
	// JWT uses raw base64url (no padding). Add padding if needed.
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}

	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("httpmw: decode JWT payload: %w", err)
	}

	var claims map[string]any
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return nil, fmt.Errorf("httpmw: unmarshal JWT claims: %w", err)
	}
	return claims, nil
}
