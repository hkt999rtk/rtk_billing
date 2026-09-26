package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/config"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentcrypto"
	"github.com/hkt999rtk/rtk_billing/internal/paymentprovider/newebpay"
	"github.com/hkt999rtk/rtk_billing/internal/paymentprovider/paypal"
	paymentSimulator "github.com/hkt999rtk/rtk_billing/internal/paymentprovider/simulator"
	"github.com/hkt999rtk/rtk_billing/internal/paymentservice"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/topupemail"
)

func main() {
	if !truthy(os.Getenv("PAYMENT_WORKER_ENABLED")) {
		log.Print("payment worker disabled")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	databaseURL, encryptionKey := strings.TrimSpace(os.Getenv("DATABASE_URL")), strings.TrimSpace(os.Getenv("PAYMENT_REFERENCE_ENCRYPTION_KEY"))
	if databaseURL == "" || encryptionKey == "" {
		log.Fatal("DATABASE_URL and PAYMENT_REFERENCE_ENCRYPTION_KEY are required")
	}
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	resolver, err := paymentcrypto.New(encryptionKey)
	if err != nil {
		log.Fatal(err)
	}
	providers, chargeEnabled, err := buildPaymentProviders(config.Config{
		Environment:             env("ENVIRONMENT", "development"),
		SimulatorEnabled:        truthy(os.Getenv("PAYMENT_SIMULATOR_ENABLED")),
		SimulatorBaseURL:        strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_BASE_URL")),
		SimulatorSharedSecret:   strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_SHARED_SECRET")),
		SimulatorRunID:          env("PAYMENT_SIMULATOR_RUN_ID", "worker"),
		SimulatorScenario:       env("PAYMENT_SIMULATOR_SCENARIO", "success"),
		NewebPayEnabled:         truthy(os.Getenv("NEWEBPAY_ENABLED")),
		NewebPayEnvironment:     env("NEWEBPAY_ENVIRONMENT", "sandbox"),
		NewebPayMerchantID:      strings.TrimSpace(os.Getenv("NEWEBPAY_MERCHANT_ID")),
		NewebPayHashKey:         os.Getenv("NEWEBPAY_HASH_KEY"),
		NewebPayHashIV:          os.Getenv("NEWEBPAY_HASH_IV"),
		NewebPayEndpointBaseURL: strings.TrimSpace(os.Getenv("NEWEBPAY_SIMULATOR_BASE_URL")),
		PayPalEnabled:           truthy(os.Getenv("PAYPAL_ENABLED")),
		PayPalEnvironment:       env("PAYPAL_ENVIRONMENT", "sandbox"),
		PayPalClientID:          strings.TrimSpace(os.Getenv("PAYPAL_CLIENT_ID")),
		PayPalClientSecret:      os.Getenv("PAYPAL_CLIENT_SECRET"),
		PayPalWebhookID:         strings.TrimSpace(os.Getenv("PAYPAL_WEBHOOK_ID")),
		PayPalReturnURL:         strings.TrimSpace(os.Getenv("PAYPAL_RETURN_URL")),
		PayPalCancelURL:         strings.TrimSpace(os.Getenv("PAYPAL_CANCEL_URL")),
		RequestTimeout:          15 * time.Second,
	})
	if err != nil {
		log.Fatal(err)
	}
	if len(providers) == 0 {
		log.Fatal("payment worker has no enabled provider")
	}
	paymentStore := paymentstore.New(db)
	service, err := paymentservice.New(paymentservice.Options{
		Store: paymentStore, Providers: providers, ReferenceResolver: resolver,
		LeaseOwner: workerIdentity(), LeaseDuration: 30 * time.Second, ReconciliationDelay: 15 * time.Second,
		BatchSize: envInt("PAYMENT_WORKER_BATCH_SIZE", 25), ChargeEnabled: chargeEnabled,
	})
	if err != nil {
		log.Fatal(err)
	}
	if truthy(os.Getenv("PAYMENT_EMAIL_ENABLED")) {
		sender, err := topupemail.New(os.Getenv("SENDMAIL_HTTP_BASE_URL"), os.Getenv("SENDMAIL_HTTP_BEARER_TOKEN"), os.Getenv("PAYMENT_EMAIL_PORTAL_BASE_URL"), os.Getenv("PAYMENT_EMAIL_FROM"))
		if err != nil {
			log.Fatal(err)
		}
		go runTopUpEmailWorker(ctx, paymentStore, sender)
	}
	if err := service.Run(ctx, 2*time.Second); err != nil {
		log.Fatal(err)
	}
}

func runTopUpEmailWorker(ctx context.Context, store *paymentstore.Store, sender *topupemail.Sender) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		messages, err := store.ClaimTopUpEmails(ctx, time.Now(), 10)
		if err != nil && ctx.Err() == nil {
			log.Printf("top-up email claim failed: %v", err)
		}
		for _, message := range messages {
			err := sender.Send(ctx, message)
			if err == nil {
				_, err = store.MarkTopUpEmailSent(ctx, message.IntentID, message.AttemptCount, time.Now())
			} else {
				_, _ = store.RetryTopUpEmail(ctx, message.IntentID, message.AttemptCount, time.Now())
			}
			if err != nil && ctx.Err() == nil {
				log.Printf("top-up email delivery failed for intent %s: %v", message.IntentID, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func buildPaymentProviders(cfg config.Config) ([]payment.PaymentProvider, map[string]bool, error) {
	providers := make([]payment.PaymentProvider, 0, 2)
	chargeEnabled := make(map[string]bool, 2)
	if cfg.SimulatorEnabled {
		if err := config.ValidateSimulatorEnvironment(cfg.Environment); err != nil {
			return nil, nil, err
		}
		provider, err := paymentSimulator.New(paymentSimulator.Config{
			BaseURL: cfg.SimulatorBaseURL, SharedSecret: cfg.SimulatorSharedSecret,
			RunID: cfg.SimulatorRunID, Scenario: cfg.SimulatorScenario, Timeout: cfg.RequestTimeout,
		})
		if err != nil {
			return nil, nil, err
		}
		providers = append(providers, provider)
		chargeEnabled[provider.Name()] = true
	}
	if cfg.NewebPayEnabled {
		provider, err := newebpay.New(newebpay.Config{
			Enabled: true, Environment: cfg.NewebPayEnvironment, MerchantID: cfg.NewebPayMerchantID,
			HashKey: cfg.NewebPayHashKey, HashIV: cfg.NewebPayHashIV,
			EndpointBaseURL: cfg.NewebPayEndpointBaseURL, Timeout: cfg.RequestTimeout,
		})
		if err != nil {
			return nil, nil, err
		}
		providers = append(providers, provider)
		chargeEnabled[provider.Name()] = false
	}
	if cfg.PayPalEnabled {
		provider, err := paypal.New(paypal.Config{Environment: cfg.PayPalEnvironment, ClientID: cfg.PayPalClientID, ClientSecret: cfg.PayPalClientSecret, WebhookID: cfg.PayPalWebhookID, ReturnURL: cfg.PayPalReturnURL, CancelURL: cfg.PayPalCancelURL})
		if err != nil {
			return nil, nil, err
		}
		providers = append(providers, provider)
		chargeEnabled[provider.Name()] = false
	}
	return providers, chargeEnabled, nil
}

func workerIdentity() string {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	return "rtk-billing-payment-worker/" + hostname
}
func truthy(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "1" || value == "true" || value == "yes"
}
func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}
