package clickup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// VerifySignature проверяет подпись вебхука ClickUp: HMAC-SHA256 от сырых байт
// тела запроса, хешированный секретом вебхука, сравнивается в hex-виде с
// заголовком X-Signature. Сравнение — через hmac.Equal, чтобы не открывать
// timing-атаку через "==".
func VerifySignature(secret string, rawBody []byte, signatureHex string) bool {
	if secret == "" || signatureHex == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rawBody)
	expected := mac.Sum(nil)

	got, err := hex.DecodeString(signatureHex)
	if err != nil {
		return false
	}

	return hmac.Equal(expected, got)
}
