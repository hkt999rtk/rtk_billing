package newebpay

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

var ErrInvalidEncryptedPayload = errors.New("invalid NewebPay encrypted payload")

func EncryptTradeInfo(plaintext, hashKey, hashIV string) (string, error) {
	block, err := newCBCBlock(hashKey, hashIV)
	if err != nil {
		return "", err
	}
	padded := addPKCS7([]byte(plaintext), block.BlockSize())
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(hashIV)).CryptBlocks(ciphertext, padded)
	return hex.EncodeToString(ciphertext), nil
}

func DecryptTradeInfo(encoded, hashKey, hashIV string) (string, error) {
	block, err := newCBCBlock(hashKey, hashIV)
	if err != nil {
		return "", err
	}
	ciphertext, err := hex.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(ciphertext) == 0 || len(ciphertext)%block.BlockSize() != 0 {
		return "", ErrInvalidEncryptedPayload
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, []byte(hashIV)).CryptBlocks(plaintext, ciphertext)
	plaintext, err = removePKCS7(plaintext, block.BlockSize())
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func newCBCBlock(hashKey, hashIV string) (cipher.Block, error) {
	if len(hashKey) != 32 || len(hashIV) != aes.BlockSize {
		return nil, fmt.Errorf("NewebPay HashKey must be 32 bytes and HashIV must be 16 bytes")
	}
	block, err := aes.NewCipher([]byte(hashKey))
	if err != nil {
		return nil, err
	}
	return block, nil
}

func addPKCS7(plaintext []byte, blockSize int) []byte {
	padding := blockSize - len(plaintext)%blockSize
	result := make([]byte, len(plaintext)+padding)
	copy(result, plaintext)
	for index := len(plaintext); index < len(result); index++ {
		result[index] = byte(padding)
	}
	return result
}

func removePKCS7(padded []byte, blockSize int) ([]byte, error) {
	if len(padded) == 0 || len(padded)%blockSize != 0 {
		return nil, ErrInvalidEncryptedPayload
	}
	padding := int(padded[len(padded)-1])
	if padding == 0 || padding > blockSize || padding > len(padded) {
		return nil, ErrInvalidEncryptedPayload
	}
	for _, value := range padded[len(padded)-padding:] {
		if int(value) != padding {
			return nil, ErrInvalidEncryptedPayload
		}
	}
	return padded[:len(padded)-padding], nil
}

func TradeSHA(tradeInfo, hashKey, hashIV string) string {
	return uppercaseSHA256("HashKey=" + hashKey + "&" + tradeInfo + "&HashIV=" + hashIV)
}

func QueryCheckValue(amountNTD int64, merchantID, merchantOrderReference, hashKey, hashIV string) string {
	values := url.Values{
		"Amt":             []string{fmt.Sprintf("%d", amountNTD)},
		"MerchantID":      []string{merchantID},
		"MerchantOrderNo": []string{merchantOrderReference},
	}
	return uppercaseSHA256("IV=" + hashIV + "&" + values.Encode() + "&Key=" + hashKey)
}

func ResponseCheckCode(fields map[string]string, hashKey, hashIV string) string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := url.Values{}
	for _, key := range keys {
		values.Set(key, fields[key])
	}
	return uppercaseSHA256("HashIV=" + hashIV + "&" + values.Encode() + "&HashKey=" + hashKey)
}

func VerifyDigest(expected, actual string) bool {
	expected = strings.ToUpper(strings.TrimSpace(expected))
	actual = strings.ToUpper(strings.TrimSpace(actual))
	if len(expected) != sha256.Size*2 || len(actual) != sha256.Size*2 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}

func uppercaseSHA256(value string) string {
	digest := sha256.Sum256([]byte(value))
	return strings.ToUpper(hex.EncodeToString(digest[:]))
}
