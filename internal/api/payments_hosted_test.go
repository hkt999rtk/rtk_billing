package api

import (
	"testing"

	"github.com/hkt999rtk/rtk_billing/internal/payment"
)

func TestValidHostedChargeActionRejectsCredentialsAndCardFields(t *testing.T) {
	valid := payment.HostedChargeResult{EndpointURL: "https://ccore.newebpay.com/MPG/mpg_gateway", Fields: map[string]string{"MerchantID": "merchant", "TradeInfo": "encrypted", "TradeSha": "digest"}}
	if !validHostedChargeAction(valid) {
		t.Fatal("valid hosted action rejected")
	}
	for _, invalid := range []payment.HostedChargeResult{
		{EndpointURL: "https://user:secret@example.com/pay", Fields: valid.Fields},
		{EndpointURL: "javascript:alert(1)", Fields: valid.Fields},
		{EndpointURL: valid.EndpointURL, Fields: map[string]string{"card_number": "4111111111111111"}},
		{EndpointURL: valid.EndpointURL, Fields: map[string]string{"CVV": "123"}},
	} {
		if validHostedChargeAction(invalid) {
			t.Fatalf("unsafe hosted action passed: %+v", invalid)
		}
	}
}
