package secretutil

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// encryptPrefixVersion is the envelope marker for ciphertext written since
// at-rest encryption was introduced. Values without it may be legacy plaintext.
const encryptPrefixVersion = "enc:v1:"

// Encryptor is the minimal at-rest cipher surface. It matches email.Encryptor
// and outbound.Encryptor structurally, so either can be passed here without an
// import cycle or a conversion.
type Encryptor interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
}

// Encrypt writes an unambiguous envelope around new ciphertext. Older channel
// secrets were stored as bare base64, which is indistinguishable by shape from
// a perfectly valid long base64 password; the prefix is what removes the
// guesswork on read.
func Encrypt(enc Encryptor, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if enc == nil {
		return "", fmt.Errorf("secret encryption is not configured")
	}
	ciphertext, err := enc.Encrypt(value)
	if err != nil {
		return "", err
	}
	return encryptPrefixVersion + ciphertext, nil
}

// DecryptOrLegacy unwraps an encrypted secret, distinguishing four cases:
//   - empty string: returns "" (caller shortcuts).
//   - enc:v1-prefixed value: decrypt strictly and propagate any corruption or
//     key mismatch.
//   - unprefixed value that looks like legacy ciphertext: try decrypting it;
//     if authentication fails, treat it as legacy plaintext because long
//     base64 passwords are otherwise impossible to represent.
//   - anything else: legacy plaintext from before encryption was introduced;
//     return as-is so existing channels keep working.
func DecryptOrLegacy(enc Encryptor, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if enc == nil {
		return value, nil
	}
	if strings.HasPrefix(value, encryptPrefixVersion) {
		plain, err := enc.Decrypt(strings.TrimPrefix(value, encryptPrefixVersion))
		if err != nil {
			return "", fmt.Errorf("decrypt secret: %w", err)
		}
		return plain, nil
	}
	// AES-GCM ciphertext minimum: 12-byte nonce + 16-byte auth tag = 28 bytes
	// raw, ~40 base64 chars. Short-and-not-base64 inputs are legacy plaintext.
	const minCipherBytes = 28
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) < minCipherBytes {
		return value, nil //nolint:nilerr // legacy-plaintext fallback is intentional; see function comment
	}
	plain, err := enc.Decrypt(value)
	if err != nil {
		return value, nil //nolint:nilerr // legacy plaintext may itself be long base64
	}
	return plain, nil
}
