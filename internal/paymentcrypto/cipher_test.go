package paymentcrypto

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testKey(character byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{character}, 32))
}

func TestPaymentMethodReferenceEncryptedAndAuthenticated(t *testing.T) {
	cipher, err := New(testKey('a'))
	if err != nil {
		t.Fatal(err)
	}
	reference := "provider-vaulted-method-secret"
	envelope, err := cipher.EncryptMethodReference(reference)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(envelope, []byte(reference)) {
		t.Fatal("ciphertext must not contain plaintext reference")
	}
	decrypted, err := cipher.ResolveMethodReference(context.Background(), envelope)
	if err != nil || decrypted != reference {
		t.Fatalf("decrypted=%q err=%v", decrypted, err)
	}

	tampered := append([]byte(nil), envelope...)
	tampered[len(tampered)-1] ^= 1
	if _, err := cipher.ResolveMethodReference(context.Background(), tampered); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("tampered ciphertext err=%v", err)
	}
	other, err := New(testKey('b'))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ResolveMethodReference(context.Background(), envelope); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("wrong key err=%v", err)
	}
}

func TestPaymentReferenceCipherRejectsMalformedConfigurationAndValues(t *testing.T) {
	for _, encoded := range []string{"not-base64", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := New(encoded); err == nil {
			t.Fatalf("key %q should fail", encoded)
		}
	}
	cipher, err := New(testKey('a'))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "  ", strings.Repeat("x", 1025)} {
		if _, err := cipher.EncryptMethodReference(value); !errors.Is(err, ErrInvalidCiphertext) {
			t.Fatalf("reference length=%d err=%v", len(value), err)
		}
	}
	for _, envelope := range [][]byte{nil, {2, 1, 2, 3}, {1, 1, 2, 3}} {
		if _, err := cipher.ResolveMethodReference(context.Background(), envelope); !errors.Is(err, ErrInvalidCiphertext) {
			t.Fatalf("envelope=%x err=%v", envelope, err)
		}
	}
}
