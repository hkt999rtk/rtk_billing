package paymentstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/testutil"
)

func TestTenantOwnerRevalidatedInsideTransactionsAcrossTransferAndReturn(t *testing.T) {
	env, account, prepare, scope := newSettlementFixture(t, 1000)
	ctx := context.Background()
	identity := billingidentity.New(env.db)
	claims, err := identity.AuthorizeOwner(ctx, account.OrganizationID, prepare.SourceUserID, prepare.OwnershipVersion)
	if err != nil {
		t.Fatal(err)
	}
	oldRequest := billingidentity.WithScope(ctx, claims)
	profiles := billingstore.New(env.db)
	profile, _, err := profiles.EnsureBillingProfile(oldRequest, account.OrganizationID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	grant := authorizeFixtureHandoff(t, env, prepare, scope)
	if _, err := identity.AuthorizeOwner(ctx, account.OrganizationID, prepare.SourceUserID, prepare.OwnershipVersion); !errors.Is(err, billingidentity.ErrTransition) {
		t.Fatalf("commit authorization exposed tenant reads: %v", err)
	}
	finish := finalizeFixtureInput(scope, prepare.TargetUserID, grant)
	if _, err := env.store.FinalizeOwnershipHandoff(ctx, finish); err != nil {
		t.Fatal(err)
	}
	for _, check := range []func() error{
		func() error { _, err := env.store.GetCommercialAccount(oldRequest, account.ID); return err },
		func() error {
			_, _, err := env.store.EnsureCommercialAccount(oldRequest, account.OrganizationID, payment.CurrencyTWD)
			return err
		},
		func() error {
			_, err := env.store.PostLedgerEntry(oldRequest, debitInput(account.ID, "stale-request-debit", 100, testTime(14, 0)))
			return err
		},
		func() error { _, err := profiles.GetBillingProfile(oldRequest, account.OrganizationID); return err },
		func() error {
			_, _, err := profiles.EnsureBillingProfile(oldRequest, account.OrganizationID, time.Now())
			return err
		},
		func() error {
			_, err := profiles.PutBillingProfile(oldRequest, billingstore.PutProfileInput{OrganizationID: account.OrganizationID, LegalName: "Old owner overwrite", Locale: "zh-TW", Timezone: "Asia/Taipei", DeliveryPreference: "portal", ExpectedVersion: profile.Version + 1})
			return err
		},
	} {
		if err := check(); !errors.Is(err, billingidentity.ErrDenied) {
			t.Fatalf("stale actor transaction accepted: %v", err)
		}
	}
	if _, err := identity.AuthorizeOwner(ctx, account.OrganizationID, prepare.SourceUserID, prepare.OwnershipVersion); !errors.Is(err, billingidentity.ErrDenied) {
		t.Fatalf("departed owner still admitted: %v", err)
	}
	newClaims, err := identity.AuthorizeOwner(ctx, account.OrganizationID, prepare.TargetUserID, prepare.OwnershipVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	newRequest := billingidentity.WithScope(ctx, newClaims)
	blank, err := profiles.GetBillingProfile(newRequest, account.OrganizationID)
	if err != nil || blank.LegalName != "" || !blank.RequiresConfiguration {
		t.Fatalf("new owner profile=%+v err=%v", blank, err)
	}
	if _, _, err := env.store.EnsureCommercialAccount(newRequest, testutil.OrganizationID("other-cloud"), payment.CurrencyTWD); !errors.Is(err, billingidentity.ErrDenied) {
		t.Fatalf("scope swapped: %v", err)
	}
	second := PrepareOwnershipHandoffInput{OperationID: testutil.OrganizationID("return-owner-access"), OrganizationID: account.OrganizationID, SourceUserID: prepare.TargetUserID, TargetUserID: prepare.SourceUserID, OwnershipVersion: prepare.OwnershipVersion + 1, Cutoff: finish.CommittedAt.Add(time.Microsecond)}
	if _, err := env.store.PrepareOwnershipHandoff(ctx, second); err != nil {
		t.Fatal(err)
	}
	secondScope := HandoffScope{OrganizationID: account.OrganizationID, OperationID: second.OperationID, OwnershipVersion: second.OwnershipVersion}
	secondGrant := authorizeFixtureHandoff(t, env, second, secondScope)
	if _, err := env.store.FinalizeOwnershipHandoff(ctx, finalizeFixtureInput(secondScope, second.TargetUserID, secondGrant)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PostLedgerEntry(oldRequest, debitInput(account.ID, "old-version-after-return", 100, testTime(14, 0))); !errors.Is(err, billingidentity.ErrVersion) {
		t.Fatalf("returning owner revived old request: %v", err)
	}
	after, err := env.store.GetCommercialAccount(ctx, account.ID)
	if err != nil || after.AvailableBalanceMinor != 1000 {
		t.Fatalf("denied writes changed balance=%+v err=%v", after, err)
	}
}

func TestTenantOwnerCannotReadUnprovenLegacyProfileOrChangeScope(t *testing.T) {
	env, account, prepare, _ := newSettlementFixture(t, 0)
	ctx := context.Background()
	profiles := billingstore.New(env.db)
	profile, _, err := profiles.EnsureBillingProfile(ctx, account.OrganizationID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE billing_profiles SET ownership_version=NULL,requires_configuration=false,legal_name='Unproven legacy payer',tax_identifier='legacy-secret' WHERE organization_id=$1`, account.OrganizationID); err != nil {
		t.Fatal(err)
	}
	claims, err := billingidentity.New(env.db).AuthorizeOwner(ctx, account.OrganizationID, prepare.SourceUserID, prepare.OwnershipVersion)
	if err != nil {
		t.Fatal(err)
	}
	tenant := billingidentity.WithScope(ctx, claims)
	if _, err := profiles.GetBillingProfile(tenant, account.OrganizationID); !errors.Is(err, billingidentity.ErrUnavailable) {
		t.Fatalf("unproven profile exposed: %v", err)
	}
	if _, _, err := profiles.EnsureBillingProfile(tenant, account.OrganizationID, time.Now()); !errors.Is(err, billingidentity.ErrUnavailable) {
		t.Fatalf("ensure bypassed unknown profile: %v", err)
	}
	other := testutil.OrganizationID("wrong-profile-cloud")
	if _, err := profiles.GetBillingProfile(tenant, other); !errors.Is(err, billingidentity.ErrDenied) {
		t.Fatalf("profile scope bypass: %v", err)
	}
	// Today's owner cannot adopt an unproven legacy profile by guessing its ETag.
	if _, err := profiles.PutBillingProfile(tenant, billingstore.PutProfileInput{OrganizationID: account.OrganizationID, LegalName: "Current owner supplied", Locale: "zh-TW", Timezone: "Asia/Taipei", DeliveryPreference: "portal", ExpectedVersion: profile.Version}); !errors.Is(err, billingidentity.ErrUnavailable) {
		t.Fatalf("unknown recipient overwritten: %v", err)
	}
	preserved, err := profiles.GetBillingProfile(ctx, account.OrganizationID)
	if err != nil || preserved.OwnershipVersion != nil || preserved.LegalName != "Unproven legacy payer" || preserved.TaxIdentifier != "legacy-secret" {
		t.Fatalf("unknown historical evidence changed=%+v err=%v", preserved, err)
	}
}
