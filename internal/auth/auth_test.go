package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("s3cret-pass")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "pbkdf2_sha256$") {
		t.Fatalf("unexpected hash format: %s", hash)
	}
	if !VerifyPassword(hash, "s3cret-pass") {
		t.Fatal("expected password to verify")
	}
	if VerifyPassword(hash, "wrong-pass") {
		t.Fatal("wrong password must not verify")
	}
	if VerifyPassword("not-a-hash", "s3cret-pass") {
		t.Fatal("malformed hash must not verify")
	}
}

func TestGenerateAPIKey(t *testing.T) {
	plain, prefix, hash, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if !strings.HasPrefix(plain, "sk-") || len(plain) != 51 {
		t.Fatalf("unexpected key shape %q (len %d)", plain, len(plain))
	}
	if !strings.HasPrefix(plain, prefix) {
		t.Fatalf("prefix %q does not match key", prefix)
	}
	if hash != HashAPIKey(plain) {
		t.Fatal("hash mismatch")
	}
	other, _, otherHash, _ := GenerateAPIKey()
	if plain == other || hash == otherHash {
		t.Fatal("keys must be unique")
	}
}

func TestSessionClaims(t *testing.T) {
	secret := "test-secret"
	claims := Claims{Sub: 7, Username: "admin", Role: "admin", IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Hour).Unix()}
	token, err := Sign(secret, claims)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	parsed, err := Parse(secret, token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.Sub != 7 || parsed.Username != "admin" || parsed.Role != "admin" {
		t.Fatalf("unexpected claims: %+v", parsed)
	}
	if _, err := Parse("other-secret", token); err == nil {
		t.Fatal("expected signature rejection")
	}
	tampered := strings.Replace(token, token[len(token)-2:], "aa", 1)
	if _, err := Parse(secret, tampered); err == nil {
		t.Fatal("expected tampered token rejection")
	}
	expired := claims
	expired.Expires = time.Now().Add(-time.Minute).Unix()
	expiredToken, _ := Sign(secret, expired)
	if _, err := Parse(secret, expiredToken); err == nil {
		t.Fatal("expected expired token rejection")
	}
}
