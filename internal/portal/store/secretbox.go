package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
)

// SecretBox encrypts values stored in the database with AES-256-GCM, using a
// key kept outside the database.
type SecretBox struct {
	aead cipher.AEAD
}

// NewSecretBox returns a SecretBox for a 32-byte key.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != 32 {
		return nil, errors.New("secret key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretBox{aead: aead}, nil
}

// Seal encrypts plaintext. The random nonce is prepended to the result.
func (b *SecretBox) Seal(plaintext []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize(), b.aead.NonceSize()+len(plaintext)+b.aead.Overhead())
	_, _ = rand.Read(nonce)
	return b.aead.Seal(nonce, nonce, plaintext, nil)
}

// Open decrypts a value produced by Seal.
func (b *SecretBox) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed value too short")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], nil)
}
