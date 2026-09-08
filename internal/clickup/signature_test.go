package clickup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature_Valid(t *testing.T) {
	body := []byte(`{"task_id":"abc123","event":"taskStatusUpdated"}`)
	secret := "s3cr3t"
	sig := sign(secret, body)

	if !VerifySignature(secret, body, sig) {
		t.Fatal("expected valid signature to verify")
	}
}

func TestVerifySignature_TamperedBody(t *testing.T) {
	body := []byte(`{"task_id":"abc123","event":"taskStatusUpdated"}`)
	secret := "s3cr3t"
	sig := sign(secret, body)

	tampered := []byte(`{"task_id":"abc999","event":"taskStatusUpdated"}`)
	if VerifySignature(secret, tampered, sig) {
		t.Fatal("expected tampered body to fail verification")
	}
}

func TestVerifySignature_WrongSecret(t *testing.T) {
	body := []byte(`{"task_id":"abc123"}`)
	sig := sign("correct-secret", body)

	if VerifySignature("wrong-secret", body, sig) {
		t.Fatal("expected wrong secret to fail verification")
	}
}

func TestVerifySignature_MalformedHex(t *testing.T) {
	body := []byte(`{"task_id":"abc123"}`)
	if VerifySignature("secret", body, "not-hex-!!!") {
		t.Fatal("expected malformed hex signature to fail verification")
	}
}

func TestVerifySignature_EmptySecretOrSignature(t *testing.T) {
	body := []byte(`{"task_id":"abc123"}`)
	if VerifySignature("", body, "aabbcc") {
		t.Fatal("expected empty secret to fail verification")
	}
	if VerifySignature("secret", body, "") {
		t.Fatal("expected empty signature to fail verification")
	}
}
