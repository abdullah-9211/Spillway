// Package secret encrypts small values, such as a tool's auth headers, before they are stored. It uses AES-256-GCM
// with a random nonce per value. The key comes from SPILLWAY_SECRET_KEY, 32 bytes in base64.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrCorrupt is returned for a value that is too short, was encrypted with another key, or was altered.
var ErrCorrupt = errors.New("secret: the value cannot be decrypted with this key")

type Box struct{ gcm cipher.AEAD }

// ParseKey decodes a base64 key and checks that it is exactly 32 bytes.
func ParseKey(b64 string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("secret key is not valid base64: %w", err)
	}
	if len(k) != 32 {
		return nil, fmt.Errorf("secret key must be 32 bytes, got %d", len(k))
	}
	return k, nil
}

func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secret key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{gcm: gcm}, nil
}

// Encrypt returns nonce || ciphertext. The context binds the value to where it is used (for example the tool's id),
// so a value copied to another row does not decrypt there.
func (b *Box) Encrypt(plain, context []byte) ([]byte, error) {
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.gcm.Seal(nonce, nonce, plain, context), nil
}

func (b *Box) Decrypt(sealed, context []byte) ([]byte, error) {
	n := b.gcm.NonceSize()
	if len(sealed) < n+b.gcm.Overhead() {
		return nil, ErrCorrupt
	}
	plain, err := b.gcm.Open(nil, sealed[:n], sealed[n:], context)
	if err != nil {
		return nil, ErrCorrupt
	}
	return plain, nil
}
