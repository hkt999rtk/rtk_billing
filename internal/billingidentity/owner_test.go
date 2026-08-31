package billingidentity

import (
	"context"
	"testing"
	"time"
)

func TestScopeContextAndProviderReconciliationAreDistinct(t *testing.T) {
	base := context.Background()
	if _, ok := FromContext(base); ok {
		t.Fatal("empty context has tenant scope")
	}
	want := Scope{OrganizationID: "org", AccountID: "account", UserID: "user", OwnershipVersion: 4, CurrentPeriodStart: time.Now()}
	if got, ok := FromContext(WithScope(base, want)); !ok || got != want {
		t.Fatalf("scope round trip: %+v %t", got, ok)
	}
	if _, ok := FromContext(ForProviderReconciliation(WithScope(base, want))); ok {
		t.Fatal("provider reconciliation retained tenant authority")
	}
}

func TestValidUUIDRequiresCanonicalNonemptyUUID(t *testing.T) {
	for _, value := range []string{"11111111-1111-1111-1111-111111111111", "00000000-0000-0000-0000-000000000000"} {
		if !ValidUUID(value) {
			t.Fatalf("canonical UUID rejected: %s", value)
		}
	}
	for _, value := range []string{"", "not-a-uuid", "11111111111111111111111111111111", "11111111-1111-1111-1111-11111111111A"} {
		if ValidUUID(value) {
			t.Fatalf("noncanonical UUID accepted: %s", value)
		}
	}
}
