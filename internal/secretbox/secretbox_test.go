package secretbox

import "testing"

func TestRoundTrip(t *testing.T) {
	c, err := New("unit-test-secret")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	encrypted, err := c.Encrypt("sk-upstream-secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if encrypted == "sk-upstream-secret" {
		t.Fatal("ciphertext must differ from plaintext")
	}
	plain, err := c.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plain != "sk-upstream-secret" {
		t.Fatalf("unexpected plaintext %q", plain)
	}
	second, _ := c.Encrypt("sk-upstream-secret")
	if second == encrypted {
		t.Fatal("nonce should randomize ciphertext")
	}
	other, _ := New("another-secret")
	if _, err := other.Decrypt(encrypted); err == nil {
		t.Fatal("expected decryption with wrong key to fail")
	}
}
