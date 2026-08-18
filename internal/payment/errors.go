package payment

import "errors"

var (
	ErrInvalidCurrency       = errors.New("invalid or unsupported currency")
	ErrInvalidAmount         = errors.New("invalid amount")
	ErrBalanceOverflow       = errors.New("balance overflow")
	ErrInvalidDirection      = errors.New("invalid ledger direction")
	ErrInvalidReason         = errors.New("invalid ledger reason")
	ErrInvalidState          = errors.New("invalid state")
	ErrInvalidTransition     = errors.New("invalid payment intent transition")
	ErrInvalidPolicy         = errors.New("invalid auto top-up policy")
	ErrPaymentMethodInactive = errors.New("payment method is inactive")
	ErrCapabilityUnsupported = errors.New("payment capability is unsupported")
)
