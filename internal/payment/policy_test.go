package payment

import (
	"errors"
	"testing"
	"time"
)

func validPolicy(now time.Time) AutoTopUpPolicy {
	return AutoTopUpPolicy{
		ID:                    "policy-1",
		AccountID:             "account-1",
		Enabled:               true,
		ThresholdMinor:        10000,
		TopUpAmountMinor:      50000,
		Currency:              CurrencyTWD,
		PaymentMethodID:       "method-1",
		DailyAttemptLimit:     3,
		DailyAmountLimitMinor: 150000,
		CooldownSeconds:       3600,
		Generation:            1,
		Version:               1,
		Armed:                 true,
		ConsentID:             "consent-1",
		CreatedAt:             now,
		UpdatedAt:             now,
	}
}

func validEvaluation(now time.Time) PolicyEvaluation {
	return PolicyEvaluation{
		BalanceMinor:            9900,
		Now:                     now,
		PaymentMethodStatus:     PaymentMethodStatusActive,
		MerchantInitiatedCharge: true,
	}
}

func TestAutoTopUpUsesStrictThresholdAndSafetyLimits(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	policy := validPolicy(now)
	if err := ValidatePolicy(policy); err != nil {
		t.Fatal(err)
	}

	evaluation := validEvaluation(now)
	if decision := EvaluateAutoTopUp(policy, evaluation); !decision.Trigger || decision.Code != PolicyDecisionTrigger {
		t.Fatalf("unexpected decision: %+v", decision)
	}

	evaluation.BalanceMinor = policy.ThresholdMinor
	if decision := EvaluateAutoTopUp(policy, evaluation); decision.Trigger || decision.Code != PolicyDecisionThresholdNotCrossed {
		t.Fatalf("equality must not trigger: %+v", decision)
	}

	evaluation = validEvaluation(now)
	evaluation.HasOpenIntent = true
	if decision := EvaluateAutoTopUp(policy, evaluation); decision.Code != PolicyDecisionOpenIntent {
		t.Fatalf("open intent decision: %+v", decision)
	}

	evaluation = validEvaluation(now)
	evaluation.AttemptsToday = policy.DailyAttemptLimit
	if decision := EvaluateAutoTopUp(policy, evaluation); decision.Code != PolicyDecisionDailyAttemptLimit {
		t.Fatalf("attempt limit decision: %+v", decision)
	}

	evaluation = validEvaluation(now)
	evaluation.AutomaticAmountTodayMinor = 100001
	if decision := EvaluateAutoTopUp(policy, evaluation); decision.Code != PolicyDecisionDailyAmountLimit {
		t.Fatalf("amount limit decision: %+v", decision)
	}
}

func TestAutoTopUpCooldownCapabilityAndMethodState(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	policy := validPolicy(now)
	last := now.Add(-time.Minute)
	policy.LastTriggeredAt = &last

	evaluation := validEvaluation(now)
	if decision := EvaluateAutoTopUp(policy, evaluation); decision.Code != PolicyDecisionCooldown {
		t.Fatalf("cooldown decision: %+v", decision)
	}

	policy.LastTriggeredAt = nil
	evaluation.PaymentMethodStatus = PaymentMethodStatusRevoked
	if decision := EvaluateAutoTopUp(policy, evaluation); decision.Code != PolicyDecisionPaymentMethodInactive {
		t.Fatalf("method decision: %+v", decision)
	}

	evaluation.PaymentMethodStatus = PaymentMethodStatusActive
	evaluation.MerchantInitiatedCharge = false
	if decision := EvaluateAutoTopUp(policy, evaluation); decision.Code != PolicyDecisionCapabilityUnsupported {
		t.Fatalf("capability decision: %+v", decision)
	}
}

func TestAutoTopUpDisabledAndDisarmedDoNotTrigger(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	policy := validPolicy(now)
	evaluation := validEvaluation(now)
	policy.Enabled = false
	if decision := EvaluateAutoTopUp(policy, evaluation); decision.Code != PolicyDecisionDisabled {
		t.Fatalf("disabled decision: %+v", decision)
	}
	policy.Enabled = true
	policy.Armed = false
	if decision := EvaluateAutoTopUp(policy, evaluation); decision.Code != PolicyDecisionNotArmed {
		t.Fatalf("disarmed decision: %+v", decision)
	}
}

func TestValidatePolicyRejectsUnboundedOrInvalidConfiguration(t *testing.T) {
	now := time.Now().UTC()
	policy := validPolicy(now)
	policy.ThresholdMinor = 0
	if !errors.Is(ValidatePolicy(policy), ErrInvalidPolicy) {
		t.Fatal("non-positive threshold should fail")
	}

	policy = validPolicy(now)
	policy.TopUpAmountMinor = 0
	if !errors.Is(ValidatePolicy(policy), ErrInvalidPolicy) {
		t.Fatal("non-positive top-up should fail")
	}

	policy = validPolicy(now)
	policy.DailyAmountLimitMinor = 0
	if !errors.Is(ValidatePolicy(policy), ErrInvalidPolicy) {
		t.Fatal("unbounded daily amount should fail")
	}

	policy = validPolicy(now)
	policy.CooldownSeconds = MinimumCooldownSeconds - 1
	if !errors.Is(ValidatePolicy(policy), ErrInvalidPolicy) {
		t.Fatal("short cooldown should fail")
	}

	policy = validPolicy(now)
	policy.PaymentMethodID = ""
	if !errors.Is(ValidatePolicy(policy), ErrInvalidPolicy) {
		t.Fatal("missing payment method should fail")
	}

	policy = validPolicy(now)
	policy.TopUpAmountMinor = policy.DailyAmountLimitMinor + 100
	if !errors.Is(ValidatePolicy(policy), ErrInvalidPolicy) {
		t.Fatal("top-up greater than daily limit should fail")
	}
}
