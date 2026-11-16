package gateway

import (
	"regexp"
	"strings"
)

var (
	// Bearer tokens in prose, JSON values or header-like text.
	bearerTokenPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	// Common API-key shaped values.
	skTokenPattern = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`)
	// JSON or header fields whose value is a credential.
	credentialFieldPattern = regexp.MustCompile(`(?i)("?(?:api[_-]?key|authorization|x-api-key)"?\s*[:=]\s*"?)((?:Bearer\s+)?[^"\s,}]+)`)
)

// redactSensitive removes channel credentials and credential-shaped values from
// text that is about to be returned to a client or persisted/logged. The
// explicit secrets are replaced first, so provider-specific key formats that do
// not match the generic patterns are still hidden.
func redactSensitive(text string, secrets ...string) string {
	if text == "" {
		return text
	}
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) >= 4 {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	text = bearerTokenPattern.ReplaceAllString(text, "Bearer [REDACTED]")
	text = skTokenPattern.ReplaceAllString(text, "sk-[REDACTED]")
	text = credentialFieldPattern.ReplaceAllString(text, "${1}[REDACTED]")
	return text
}
