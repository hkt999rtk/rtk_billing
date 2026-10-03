package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/hkt999rtk/rtk_billing/internal/rawretention"
)

type RawRetentionConfig struct {
	Enabled            bool
	Environment        string
	ListenAddr         string
	FinancialToken     string
	RecoveryToken      string
	ControllerToken    string
	AuthorityReadToken string
	LoggerBaseURL      string
	LoggerToken        string
	ConsumerID         string
	ConsumerBaseURL    string
	ConsumerToken      string
	VerifierKeyID      string
	VerifierPublicKey  ed25519.PublicKey
}

func loadRawRetention(cfg *Config) error {
	rawEnabled := env("BILLING_RAW_RETENTION_ENABLED", "false")
	if rawEnabled != "true" && rawEnabled != "false" {
		return errors.New("BILLING_RAW_RETENTION_ENABLED must be true or false")
	}
	if rawEnabled == "false" {
		return nil
	}
	logicalEnv := strings.ToLower(env("BILLING_RAW_RETENTION_ENVIRONMENT", cfg.Environment))
	switch logicalEnv {
	case "development":
		logicalEnv = "dev"
	case "production":
		logicalEnv = "prod"
	}
	if logicalEnv != "dev" && logicalEnv != "staging" && logicalEnv != "prod" {
		return errors.New("raw retention requires a registered dev, staging, or prod environment")
	}
	r := RawRetentionConfig{Enabled: true, Environment: logicalEnv,
		ListenAddr:         strings.TrimSpace(env("BILLING_RAW_RETENTION_LISTEN_ADDR", ":8081")),
		FinancialToken:     strings.TrimSpace(os.Getenv("BILLING_RAW_RETENTION_FINANCIAL_TOKEN")),
		RecoveryToken:      strings.TrimSpace(os.Getenv("BILLING_RAW_RETENTION_RECOVERY_TOKEN")),
		ControllerToken:    strings.TrimSpace(os.Getenv("BILLING_RAW_RETENTION_CONTROLLER_TOKEN")),
		AuthorityReadToken: strings.TrimSpace(os.Getenv("BILLING_RAW_RETENTION_AUTHORITY_READ_TOKEN")),
		LoggerBaseURL:      strings.TrimSpace(os.Getenv("BILLING_RAW_RETENTION_LOGGER_BASE_URL")),
		LoggerToken:        strings.TrimSpace(os.Getenv("BILLING_RAW_RETENTION_LOGGER_TOKEN")),
		ConsumerID:         strings.TrimSpace(os.Getenv("BILLING_RAW_RETENTION_CONSUMER_ID")),
		ConsumerBaseURL:    strings.TrimSpace(os.Getenv("BILLING_RAW_RETENTION_CONSUMER_BASE_URL")),
		ConsumerToken:      strings.TrimSpace(os.Getenv("BILLING_RAW_RETENTION_CONSUMER_TOKEN")),
		VerifierKeyID:      strings.TrimSpace(os.Getenv("BILLING_RAW_RETENTION_VERIFIER_KEY_ID")),
	}
	host, port, err := net.SplitHostPort(r.ListenAddr)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 1 || portNumber > 65535 || (host != "" && net.ParseIP(host) == nil) || port == cfg.Port {
		return errors.New("raw retention requires a separate valid IP listen address and port")
	}
	public, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("BILLING_RAW_RETENTION_VERIFIER_PUBLIC_KEY")))
	if err != nil || len(public) != ed25519.PublicKeySize || r.VerifierKeyID == "" || len(r.VerifierKeyID) > 128 {
		return errors.New("raw retention requires an approved Ed25519 verifier key id and base64 public key")
	}
	r.VerifierPublicKey = ed25519.PublicKey(public)
	newTokens := []string{r.FinancialToken, r.RecoveryToken, r.ControllerToken, r.AuthorityReadToken, r.LoggerToken, r.ConsumerToken}
	for _, token := range newTokens {
		if len(token) < 32 || strings.ContainsAny(token, " \t\r\n") {
			return errors.New("raw retention requires six distinct 32+ character service credentials")
		}
	}
	all := append(append([]string{}, newTokens...), cfg.ServiceToken, cfg.InternalToken, cfg.HandoffToken, cfg.CloudCreationToken, cfg.BillingDebitToken,
		cfg.OTAPlatformSealToken, cfg.OTAProducerSealToken, cfg.OTAGrantHistoryToken, cfg.SimulatorSharedSecret, cfg.SimulatorCallbackSecret,
		cfg.NewebPayHashKey, cfg.PaymentReferenceEncryptionKey, cfg.PayPalClientSecret)
	if credentialReuse(all...) {
		return errors.New("raw retention credentials must be distinct from each other and existing authorities")
	}
	client := &rawretention.EvidenceClient{Consumers: map[string]rawretention.Consumer{r.ConsumerID: {ID: r.ConsumerID, BaseURL: r.ConsumerBaseURL, Token: r.ConsumerToken}}, LoggerBaseURL: r.LoggerBaseURL, LoggerToken: r.LoggerToken}
	if client.Validate() != nil {
		return errors.New("raw retention requires a configured authenticated Logger origin and consumer origin")
	}
	cfg.RawRetention = r
	return nil
}
