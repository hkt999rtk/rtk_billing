package newebpay

import (
	"crypto/aes"
	"encoding/hex"
	"strings"
	"testing"
)

const (
	officialHashKey   = "Fs5cX1TGqYM2PpdbE14a9H83YQSQF5jn"
	officialHashIV    = "C6AcmfqJILwgnhIP"
	officialTradeInfo = "f79eac33c4f3245d58f17b544c5d38b09457a6d77e77bae6f10fcc7236fe153ccef1a80" +
		"001c0746afc063a7570f80ad970d8a32c72332c9ec5547410188007876bdca2bafa52d0" +
		"7d31b6b183f2204d6e4feee6d245e286ab198cf95422ad5843c7696fc943cbb65979ad2" +
		"07607d4b5d97dac4a90ccd5e7a37adb7d7062e838be09d94e8c5dfa145c048e17feabe5" +
		"8c2e310792f0f50f5af32961ffb07ff6649ae1021ad558242551de5f09316e3182e1987" +
		"75e5d1ad5b66a70be290004de750fa85d86b0c2f087b40005d89e048be2ab6fd83f1c52" +
		"2494c093426a10a1f73fe4"
)

func TestOfficialNDNF123TradeSHAVector(t *testing.T) {
	if got := TradeSHA(officialTradeInfo, officialHashKey, officialHashIV); got != "84E4D9F96537E029F8450BE1E759080F9AF6995921B7F6F9AAFDDD2C36E7B287" {
		t.Fatalf("TradeSHA=%s", got)
	}
	if got := QueryCheckValue(30, "MS127874575", "Vanespl_ec_1695795668", officialHashKey, officialHashIV); got != "CD326F689018E7862727547F85CECD7DD7AE0FDB7782DE2C1E46B4417245B51F" {
		t.Fatalf("CheckValue=%s", got)
	}
}

func TestTradeInfoCBCPKCS7RoundTripAndTamperRejection(t *testing.T) {
	plaintext := "MerchantID=MS127874575&RespondType=String&TimeStamp=1695795410&Version=2.3&MerchantOrderNo=order_1&Amt=30"
	encrypted, err := EncryptTradeInfo(plaintext, officialHashKey, officialHashIV)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encrypted, "MerchantID") || len(encrypted)%32 != 0 {
		t.Fatalf("unexpected ciphertext %q", encrypted)
	}
	decrypted, err := DecryptTradeInfo(encrypted, officialHashKey, officialHashIV)
	if err != nil || decrypted != plaintext {
		t.Fatalf("decrypted=%q err=%v", decrypted, err)
	}
	tamperedBytes, err := hex.DecodeString(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	// Flip the byte that determines the final plaintext padding length.
	tamperedBytes[len(tamperedBytes)-aes.BlockSize-1] ^= 1
	tampered := hex.EncodeToString(tamperedBytes)
	if _, err := DecryptTradeInfo(tampered, officialHashKey, officialHashIV); err == nil {
		t.Fatal("tampered padding should fail")
	}
	for _, malformed := range []string{"not-hex", "00", strings.Repeat("00", 16)} {
		if _, err := DecryptTradeInfo(malformed, "short", officialHashIV); err == nil {
			t.Fatalf("malformed ciphertext %q should fail", malformed)
		}
	}
}

func TestResponseCheckCodeAndConstantTimeVerification(t *testing.T) {
	fields := map[string]string{
		"Amt": "10", "MerchantID": "MS12345678",
		"MerchantOrderNo": "MyCompanyOrder_1638423361", "TradeNo": "21120214151152468",
	}
	code := ResponseCheckCode(fields, officialHashKey, officialHashIV)
	if !VerifyDigest(code, strings.ToLower(code)) {
		t.Fatal("case-insensitive hex digest should verify")
	}
	if VerifyDigest(code, strings.Repeat("0", 64)) || VerifyDigest(code, "short") {
		t.Fatal("different digest must not verify")
	}
}
