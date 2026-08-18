package payment

import "time"

const MinimumCooldownSeconds int64 = 300

type PolicyEvaluation struct {
	BalanceMinor              int64
	Now                       time.Time
	AttemptsToday             int
	AutomaticAmountTodayMinor int64
	HasOpenIntent             bool
	PaymentMethodStatus       PaymentMethodStatus
	MerchantInitiatedCharge   bool
}

type PolicyDecisionCode string

const (
	PolicyDecisionTrigger               PolicyDecisionCode = "trigger"
	PolicyDecisionDisabled              PolicyDecisionCode = "disabled"
	PolicyDecisionNotArmed              PolicyDecisionCode = "not_armed"
	PolicyDecisionThresholdNotCrossed   PolicyDecisionCode = "threshold_not_crossed"
	PolicyDecisionOpenIntent            PolicyDecisionCode = "open_intent"
	PolicyDecisionCooldown              PolicyDecisionCode = "cooldown"
	PolicyDecisionDailyAttemptLimit     PolicyDecisionCode = "daily_attempt_limit"
	PolicyDecisionDailyAmountLimit      PolicyDecisionCode = "daily_amount_limit"
	PolicyDecisionPaymentMethodInactive PolicyDecisionCode = "payment_method_inactive"
	PolicyDecisionCapabilityUnsupported PolicyDecisionCode = "capability_unsupported"
)

type PolicyDecision struct {
	Trigger bool               `json:"trigger"`
	Code    PolicyDecisionCode `json:"code"`
}

func ValidatePolicy(policy AutoTopUpPolicy) error {
	if err := ValidateChargeAmount(policy.Currency, policy.ThresholdMinor); err != nil {
		return ErrInvalidPolicy
	}
	if err := ValidateChargeAmount(policy.Currency, policy.TopUpAmountMinor); err != nil {
		return ErrInvalidPolicy
	}
	if policy.PaymentMethodID == "" || policy.ConsentID == "" ||
		policy.DailyAttemptLimit <= 0 || policy.DailyAttemptLimit > 10 ||
		policy.DailyAmountLimitMinor <= 0 || policy.CooldownSeconds < MinimumCooldownSeconds ||
		policy.Generation <= 0 || policy.Version <= 0 || policy.ConsecutiveFailureCount < 0 {
		return ErrInvalidPolicy
	}
	if policy.TopUpAmountMinor > policy.DailyAmountLimitMinor {
		return ErrInvalidPolicy
	}
	return nil
}

func EvaluateAutoTopUp(policy AutoTopUpPolicy, evaluation PolicyEvaluation) PolicyDecision {
	if !policy.Enabled {
		return PolicyDecision{Code: PolicyDecisionDisabled}
	}
	if !policy.Armed {
		return PolicyDecision{Code: PolicyDecisionNotArmed}
	}
	if evaluation.BalanceMinor >= policy.ThresholdMinor {
		return PolicyDecision{Code: PolicyDecisionThresholdNotCrossed}
	}
	if evaluation.HasOpenIntent {
		return PolicyDecision{Code: PolicyDecisionOpenIntent}
	}
	if evaluation.PaymentMethodStatus != PaymentMethodStatusActive {
		return PolicyDecision{Code: PolicyDecisionPaymentMethodInactive}
	}
	if !evaluation.MerchantInitiatedCharge {
		return PolicyDecision{Code: PolicyDecisionCapabilityUnsupported}
	}
	if policy.LastTriggeredAt != nil && evaluation.Now.Before(policy.LastTriggeredAt.Add(time.Duration(policy.CooldownSeconds)*time.Second)) {
		return PolicyDecision{Code: PolicyDecisionCooldown}
	}
	if evaluation.AttemptsToday >= policy.DailyAttemptLimit {
		return PolicyDecision{Code: PolicyDecisionDailyAttemptLimit}
	}
	if evaluation.AutomaticAmountTodayMinor > policy.DailyAmountLimitMinor-policy.TopUpAmountMinor {
		return PolicyDecision{Code: PolicyDecisionDailyAmountLimit}
	}
	return PolicyDecision{Trigger: true, Code: PolicyDecisionTrigger}
}
