package payment

import (
	"errors"
	"testing"
)

func TestIntentTransitionsPreserveUnknownReconciliation(t *testing.T) {
	legal := [][2]PaymentIntentState{
		{PaymentIntentStateCreated, PaymentIntentStateProcessing},
		{PaymentIntentStateProcessing, PaymentIntentStateAuthorized},
		{PaymentIntentStateAuthorized, PaymentIntentStateSucceeded},
		{PaymentIntentStateProcessing, PaymentIntentStateUnknown},
		{PaymentIntentStateUnknown, PaymentIntentStateSucceeded},
		{PaymentIntentStateRequiresAction, PaymentIntentStateProcessing},
	}
	for _, pair := range legal {
		if err := ValidateIntentTransition(pair[0], pair[1]); err != nil {
			t.Fatalf("%s -> %s: %v", pair[0], pair[1], err)
		}
	}

	illegal := [][2]PaymentIntentState{
		{PaymentIntentStateCreated, PaymentIntentStateSucceeded},
		{PaymentIntentStateSucceeded, PaymentIntentStateProcessing},
		{PaymentIntentStateFailed, PaymentIntentStateProcessing},
		{PaymentIntentStateUnknown, PaymentIntentStateProcessing},
	}
	for _, pair := range illegal {
		if !errors.Is(ValidateIntentTransition(pair[0], pair[1]), ErrInvalidTransition) {
			t.Fatalf("%s -> %s should be invalid", pair[0], pair[1])
		}
	}
}

func TestIntentTerminalStates(t *testing.T) {
	for _, state := range []PaymentIntentState{PaymentIntentStateSucceeded, PaymentIntentStateFailed, PaymentIntentStateCanceled} {
		if !IntentStateTerminal(state) {
			t.Fatalf("%s should be terminal", state)
		}
	}
	if IntentStateTerminal(PaymentIntentStateUnknown) {
		t.Fatal("unknown must remain reconcilable")
	}
	for _, state := range []PaymentIntentState{
		PaymentIntentStateCreated,
		PaymentIntentStateProcessing,
		PaymentIntentStateAuthorized,
		PaymentIntentStateRequiresAction,
		PaymentIntentStateUnknown,
		PaymentIntentStateSucceeded,
		PaymentIntentStateFailed,
		PaymentIntentStateCanceled,
	} {
		if !ValidIntentState(state) {
			t.Fatalf("%s should be valid", state)
		}
	}
	if ValidIntentState("lost") {
		t.Fatal("unknown state must be invalid")
	}
	if !errors.Is(ValidateIntentTransition("lost", PaymentIntentStateFailed), ErrInvalidState) {
		t.Fatal("invalid source state should be rejected")
	}
}
