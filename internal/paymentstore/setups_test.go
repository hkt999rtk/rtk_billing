package paymentstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

func TestPaymentMethodSetupValidationRejectsUnsafeOrIncompleteInput(t *testing.T) {
	store := &Store{}
	if _, err := store.BeginPaymentMethodSetup(context.Background(), BeginPaymentMethodSetupInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("empty begin error=%v", err)
	}
	if _, err := store.CompletePaymentMethodSetup(context.Background(), CompletePaymentMethodSetupInput{
		AccountID: "account-1", SessionID: "session-1", State: payment.PaymentIntentStateSucceeded,
		HostedURLSHA256: strings.Repeat("a", 64),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("plaintext-free references are required for success: %v", err)
	}
	if _, err := store.CompletePaymentMethodSetup(context.Background(), CompletePaymentMethodSetupInput{
		AccountID: "account-1", SessionID: "session-1", State: payment.PaymentIntentStateRequiresAction,
		HostedURLSHA256: strings.Repeat("a", 64), ProviderMethodRefCiphertext: []byte("must-not-be-stored-yet"),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending setup must reject provider references: %v", err)
	}
	if _, err := store.CompletePaymentMethodSetup(context.Background(), CompletePaymentMethodSetupInput{
		AccountID: "account-1", SessionID: "session-1", State: payment.PaymentIntentStateCanceled,
		HostedURLSHA256: strings.Repeat("a", 64),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("unsupported setup state must fail: %v", err)
	}
	if _, err := store.CompletePaymentMethodSetup(context.Background(), CompletePaymentMethodSetupInput{
		AccountID: "account-1", SessionID: "session-1", State: payment.PaymentIntentStateFailed,
		HostedURLSHA256: strings.Repeat("a", 64), LastFour: "12x4",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("unsafe display metadata must fail: %v", err)
	}
	if validLowerSHA256(strings.Repeat("A", 64)) || validLowerSHA256(strings.Repeat("g", 64)) || validLowerSHA256("short") {
		t.Fatal("only a canonical lowercase SHA-256 digest is valid")
	}
	if !validLowerSHA256(strings.Repeat("a", 64)) {
		t.Fatal("canonical lowercase SHA-256 digest should be valid")
	}
}
