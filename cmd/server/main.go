package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/accessstore"
	"github.com/hkt999rtk/rtk_billing/internal/api"
	"github.com/hkt999rtk/rtk_billing/internal/auditstore"
	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
	"github.com/hkt999rtk/rtk_billing/internal/billingservice"
	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
	"github.com/hkt999rtk/rtk_billing/internal/config"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/otagrant"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentcrypto"
	"github.com/hkt999rtk/rtk_billing/internal/paymentprovider/newebpay"
	"github.com/hkt999rtk/rtk_billing/internal/paymentprovider/paypal"
	paymentSimulator "github.com/hkt999rtk/rtk_billing/internal/paymentprovider/simulator"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/rawretention"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	migrateOnStartup, err := shouldMigrateOnStartup(os.Getenv("BILLING_DB_MIGRATE_ON_STARTUP"))
	if err != nil {
		log.Fatal(err)
	}
	if migrateOnStartup {
		if err := database.Migrate(ctx, db); err != nil {
			log.Fatal(err)
		}
	}

	audit := auditstore.New(db)
	server, err := api.New(api.Options{ServiceToken: cfg.ServiceToken, InternalToken: cfg.InternalToken, Audit: api.AuditAdapter{Store: audit}, Access: accessstore.New(db), Ownership: billingidentity.New(db)})
	if err != nil {
		log.Fatal(err)
	}

	paymentStore := paymentstore.New(db)
	providers := make([]payment.PaymentProvider, 0, 2)
	if cfg.SimulatorEnabled {
		provider, providerErr := paymentSimulator.New(paymentSimulator.Config{BaseURL: cfg.SimulatorBaseURL, SharedSecret: cfg.SimulatorSharedSecret, RunID: cfg.SimulatorRunID, Scenario: cfg.SimulatorScenario, Timeout: cfg.RequestTimeout})
		if providerErr != nil {
			log.Fatal(providerErr)
		}
		providers = append(providers, provider)
	}
	if cfg.NewebPayEnabled {
		provider, providerErr := newebpay.New(newebpay.Config{Enabled: true, Environment: cfg.NewebPayEnvironment, MerchantID: cfg.NewebPayMerchantID, HashKey: cfg.NewebPayHashKey, HashIV: cfg.NewebPayHashIV, EndpointBaseURL: cfg.NewebPayEndpointBaseURL, Timeout: cfg.RequestTimeout})
		if providerErr != nil {
			log.Fatal(providerErr)
		}
		providers = append(providers, provider)
	}
	if cfg.PayPalEnabled {
		provider, providerErr := paypal.New(paypal.Config{Environment: cfg.PayPalEnvironment, ClientID: cfg.PayPalClientID, ClientSecret: cfg.PayPalClientSecret, WebhookID: cfg.PayPalWebhookID, ReturnURL: cfg.PayPalReturnURL, CancelURL: cfg.PayPalCancelURL})
		if providerErr != nil {
			log.Fatal(providerErr)
		}
		providers = append(providers, provider)
	}
	var protector api.PaymentReferenceProtector
	if cfg.PaymentReferenceEncryptionKey != "" {
		protector, err = paymentcrypto.New(cfg.PaymentReferenceEncryptionKey)
		if err != nil {
			log.Fatal(err)
		}
	}
	if err := server.ConfigurePayments(api.PaymentAPIOptions{Store: paymentStore, Providers: providers, ReferenceProtector: protector, BillingDebitToken: cfg.BillingDebitToken, BillingDebitSource: cfg.BillingDebitSource, SimulatorCallbackSecret: cfg.SimulatorCallbackSecret, HostedChargeNotifyURL: cfg.NewebPayNotifyURL, HostedChargeReturnURL: cfg.NewebPayReturnURL, PayPalAfterReturnURL: cfg.PayPalAfterReturnURL}); err != nil {
		log.Fatal(err)
	}
	// An unset dedicated credential leaves all handoff routes absent. Never
	// reuse tenant/pricing/debit authority or enable migration bootstrap here.
	if cfg.HandoffToken != "" {
		if err := server.ConfigureHandoff(api.HandoffAPIOptions{Token: cfg.HandoffToken, Store: paymentStore}); err != nil {
			log.Fatal(err)
		}
	}
	if cfg.CloudCreationToken != "" {
		if err := server.ConfigureCloudCreation(api.CloudCreationAPIOptions{Token: cfg.CloudCreationToken, Store: paymentStore}); err != nil {
			log.Fatal(err)
		}
	}
	billingStore := billingstore.New(db)
	if cfg.OTAGrantHistoryBaseURL != "" {
		verifier, err := otagrant.NewClient(cfg.OTAGrantHistoryBaseURL, cfg.OTAGrantHistoryToken, nil)
		if err != nil {
			log.Fatal(err)
		}
		billingStore.SetOTAGrantVerifier(verifier)
		billingStore.SetOTATierVerifier(verifier)
	}
	if cfg.OTAPlatformSealToken != "" {
		if err := server.ConfigureOTAPeriodSeals(api.OTAPeriodSealAPIOptions{
			PlatformToken: cfg.OTAPlatformSealToken, ProducerToken: cfg.OTAProducerSealToken, Store: billingStore,
		}); err != nil {
			log.Fatal(err)
		}
	}
	billingService, err := billingservice.New(billingservice.Options{Store: billingStore, PaymentStore: paymentStore})
	if err != nil {
		log.Fatal(err)
	}
	if err := server.ConfigureBilling(api.BillingAPIOptions{Store: billingStore, Service: billingService}); err != nil {
		log.Fatal(err)
	}
	if cfg.RawRetention.Enabled {
		r := cfg.RawRetention
		evidence := &rawretention.EvidenceClient{
			Consumers:     map[string]rawretention.Consumer{r.ConsumerID: {ID: r.ConsumerID, BaseURL: r.ConsumerBaseURL, Token: r.ConsumerToken}},
			LoggerBaseURL: r.LoggerBaseURL, LoggerToken: r.LoggerToken,
		}
		authority, err := rawretention.New(db, evidence, map[string]ed25519.PublicKey{r.VerifierKeyID: r.VerifierPublicKey}, r.Environment)
		if err != nil {
			log.Fatal(err)
		}
		if err := server.ConfigureRawRetention(api.RawRetentionAPIOptions{FinancialToken: r.FinancialToken, RecoveryToken: r.RecoveryToken,
			ControllerToken: r.ControllerToken, AuthorityReadToken: r.AuthorityReadToken, Store: authority}); err != nil {
			log.Fatal(err)
		}
	}

	log.Printf("rtk_billing listening on :%s", cfg.Port)
	servers := []*http.Server{{Addr: ":" + cfg.Port, Handler: server.Router(), ReadHeaderTimeout: 10 * time.Second}}
	if cfg.RawRetention.Enabled {
		log.Printf("rtk_billing raw retention private listener on %s", cfg.RawRetention.ListenAddr)
		servers = append(servers, &http.Server{Addr: cfg.RawRetention.ListenAddr, Handler: server.RawRetentionRouter(),
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 65 * time.Second, WriteTimeout: 75 * time.Second, IdleTimeout: 60 * time.Second})
	}
	if err := serveHTTPServers(ctx, servers...); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func shouldMigrateOnStartup(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "", "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("BILLING_DB_MIGRATE_ON_STARTUP must be true or false")
	}
}
