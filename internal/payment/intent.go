package payment

func ValidIntentState(state PaymentIntentState) bool {
	switch state {
	case PaymentIntentStateCreated, PaymentIntentStateProcessing,
		PaymentIntentStateAuthorized, PaymentIntentStateRequiresAction,
		PaymentIntentStateUnknown, PaymentIntentStateSucceeded,
		PaymentIntentStateFailed, PaymentIntentStateCanceled:
		return true
	default:
		return false
	}
}

func IntentStateTerminal(state PaymentIntentState) bool {
	return state == PaymentIntentStateSucceeded || state == PaymentIntentStateFailed || state == PaymentIntentStateCanceled
}

func CanTransitionIntent(from, to PaymentIntentState) bool {
	if from == to {
		return true
	}
	switch from {
	case PaymentIntentStateCreated:
		return to == PaymentIntentStateProcessing || to == PaymentIntentStateCanceled
	case PaymentIntentStateProcessing:
		return to == PaymentIntentStateAuthorized || to == PaymentIntentStateRequiresAction ||
			to == PaymentIntentStateUnknown || to == PaymentIntentStateSucceeded ||
			to == PaymentIntentStateFailed || to == PaymentIntentStateCanceled
	case PaymentIntentStateAuthorized:
		return to == PaymentIntentStateSucceeded || to == PaymentIntentStateUnknown ||
			to == PaymentIntentStateFailed || to == PaymentIntentStateCanceled
	case PaymentIntentStateRequiresAction:
		return to == PaymentIntentStateProcessing || to == PaymentIntentStateAuthorized ||
			to == PaymentIntentStateSucceeded || to == PaymentIntentStateUnknown ||
			to == PaymentIntentStateFailed || to == PaymentIntentStateCanceled
	case PaymentIntentStateUnknown:
		return to == PaymentIntentStateAuthorized || to == PaymentIntentStateSucceeded ||
			to == PaymentIntentStateFailed || to == PaymentIntentStateCanceled
	default:
		return false
	}
}

func ValidateIntentTransition(from, to PaymentIntentState) error {
	if !ValidIntentState(from) || !ValidIntentState(to) {
		return ErrInvalidState
	}
	if !CanTransitionIntent(from, to) {
		return ErrInvalidTransition
	}
	return nil
}
