package paymentcrypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const envelopeVersion byte = 1

var ErrInvalidCiphertext = errors.New("invalid payment reference ciphertext")

type Cipher struct {
	aead cipher.AEAD
	rand io.Reader
}

func New(encodedKey string) (*Cipher, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encodedKey))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("payment reference encryption key must be base64 encoded 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead, rand: rand.Reader}, nil
}

func (c *Cipher) EncryptMethodReference(reference string) ([]byte, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" || len(reference) > 1024 {
		return nil, ErrInvalidCiphertext
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(c.rand, nonce); err != nil {
		return nil, err
	}
	envelope := make([]byte, 1, 1+len(nonce)+len(reference)+c.aead.Overhead())
	envelope[0] = envelopeVersion
	envelope = append(envelope, nonce...)
	envelope = c.aead.Seal(envelope, nonce, []byte(reference), []byte("rtk-account-manager/payment-method-reference/v1"))
	return envelope, nil
}

func (c *Cipher) ResolveMethodReference(_ context.Context, envelope []byte) (string, error) {
	minimum := 1 + c.aead.NonceSize() + c.aead.Overhead()
	if len(envelope) < minimum || envelope[0] != envelopeVersion {
		return "", ErrInvalidCiphertext
	}
	nonce := envelope[1 : 1+c.aead.NonceSize()]
	plaintext, err := c.aead.Open(nil, nonce, envelope[1+c.aead.NonceSize():], []byte("rtk-account-manager/payment-method-reference/v1"))
	if err != nil {
		return "", ErrInvalidCiphertext
	}
	reference := string(plaintext)
	if strings.TrimSpace(reference) == "" || len(reference) > 1024 {
		return "", ErrInvalidCiphertext
	}
	return reference, nil
}
