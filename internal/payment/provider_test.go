package payment

import (
	"errors"
	"strings"
	"testing"
)

func TestProviderErrorNormalizationDoesNotExposeUnsafeCode(t *testing.T) {
	var nilProviderError *ProviderError
	if nilProviderError.Error() != "" {
		t.Fatal("nil provider error should render safely")
	}
	if got := (&ProviderError{Kind: ProviderErrorTemporary}).Error(); got != "payment provider temporary" {
		t.Fatalf("provider error without code=%q", got)
	}
	err := NewProviderError(ProviderErrorAuthentication, "  SECRET token=value  ", false, errors.New("raw provider body"))
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatal("expected typed provider error")
	}
	if providerErr.Code != "redacted" || strings.Contains(providerErr.Error(), "token") || !errors.Is(err, providerErr.Cause) {
		t.Fatalf("unsafe provider error: %#v %q", providerErr, providerErr.Error())
	}
	longCode := strings.Repeat("a", 80)
	if got := NormalizeProviderCode(longCode); len(got) != 64 {
		t.Fatalf("normalized code length=%d", len(got))
	}
}

func TestProviderErrorMapsAmbiguousCallsToUnknown(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want PaymentIntentState
	}{
		{name: "declined before send", err: NewProviderError(ProviderErrorDeclined, "declined", false, nil), want: PaymentIntentStateFailed},
		{name: "invalid request", err: NewProviderError(ProviderErrorInvalidRequest, "invalid", false, nil), want: PaymentIntentStateFailed},
		{name: "requires action", err: NewProviderError(ProviderErrorRequiresAction, "otp", true, nil), want: PaymentIntentStateRequiresAction},
		{name: "declined after send", err: NewProviderError(ProviderErrorDeclined, "ambiguous", true, nil), want: PaymentIntentStateUnknown},
		{name: "ordinary error", err: errors.New("network"), want: PaymentIntentStateUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StateForProviderError(tc.err); got != tc.want {
				t.Fatalf("state=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestValidateProviderResultAcceptsOnlyConclusiveOrReconcilableStates(t *testing.T) {
	for _, state := range []PaymentIntentState{
		PaymentIntentStateAuthorized, PaymentIntentStateRequiresAction,
		PaymentIntentStateUnknown, PaymentIntentStateSucceeded,
		PaymentIntentStateFailed, PaymentIntentStateCanceled,
	} {
		if err := ValidateProviderResult(ProviderResult{State: state}); err != nil {
			t.Fatalf("state %s: %v", state, err)
		}
	}
	if !errors.Is(ValidateProviderResult(ProviderResult{State: PaymentIntentStateProcessing}), ErrInvalidState) {
		t.Fatal("processing is not a provider result")
	}
}
