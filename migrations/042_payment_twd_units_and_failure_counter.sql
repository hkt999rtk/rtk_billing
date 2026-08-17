-- TWD is a zero-decimal currency in the commercial balance contract:
-- one amount_minor unit represents NT$1. Track automatic top-up failures on
-- the policy so the third consecutive terminal failure can disable it.

ALTER TABLE auto_topup_policies
    DROP CONSTRAINT IF EXISTS auto_topup_policy_twd_amounts_check;

ALTER TABLE payment_intents
    DROP CONSTRAINT IF EXISTS payment_intents_twd_amount_check;

ALTER TABLE auto_topup_policies
    ADD COLUMN IF NOT EXISTS consecutive_failure_count INTEGER NOT NULL DEFAULT 0;

ALTER TABLE auto_topup_policies
    DROP CONSTRAINT IF EXISTS auto_topup_policy_failure_count_check;

ALTER TABLE auto_topup_policies
    ADD CONSTRAINT auto_topup_policy_failure_count_check
    CHECK (consecutive_failure_count >= 0);
