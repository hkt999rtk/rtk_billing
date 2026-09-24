-- TWD remains the only transactional currency. Preserve a searchable history
-- when a pricing version is retired; no USD/CNY account or invoice is enabled.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pricing_plan_versions WHERE currency <> 'TWD')
       OR EXISTS (SELECT 1 FROM billing_periods WHERE currency <> 'TWD')
       OR EXISTS (SELECT 1 FROM billing_invoices WHERE currency <> 'TWD')
       OR EXISTS (SELECT 1 FROM commercial_accounts WHERE currency <> 'TWD') THEN
        RAISE EXCEPTION 'unsupported non-TWD monetary rows require manual review';
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS pricing_plan_currency_history_idx
    ON pricing_plan_versions (currency, effective_from DESC, effective_until)
    WHERE status IN ('active', 'retired');
