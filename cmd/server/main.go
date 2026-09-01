package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/hkt999rtk/rtk_billing/internal/accessstore"
	"github.com/hkt999rtk/rtk_billing/internal/api"
	"github.com/hkt999rtk/rtk_billing/internal/auditstore"
	"github.com/hkt999rtk/rtk_billing/internal/billingidentity"
	"github.com/hkt999rtk/rtk_billing/internal/billingservice"
	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
	"github.com/hkt999rtk/rtk_billing/internal/config"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentcrypto"
	"github.com/hkt999rtk/rtk_billing/internal/paymentprovider/newebpay"
	paymentSimulator "github.com/hkt999rtk/rtk_billing/internal/paymentprovider/simulator"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		log.Fatal(err)
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
	var protector api.PaymentReferenceProtector
	if cfg.PaymentReferenceEncryptionKey != "" {
		protector, err = paymentcrypto.New(cfg.PaymentReferenceEncryptionKey)
		if err != nil {
			log.Fatal(err)
		}
	}
	if err := server.ConfigurePayments(api.PaymentAPIOptions{Store: paymentStore, Providers: providers, ReferenceProtector: protector, BillingDebitToken: cfg.BillingDebitToken, BillingDebitSource: cfg.BillingDebitSource, SimulatorCallbackSecret: cfg.SimulatorCallbackSecret, HostedChargeNotifyURL: cfg.NewebPayNotifyURL, HostedChargeReturnURL: cfg.NewebPayReturnURL}); err != nil {
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
	billingService, err := billingservice.New(billingservice.Options{Store: billingStore, PaymentStore: paymentStore})
	if err != nil {
		log.Fatal(err)
	}
	if err := server.ConfigureBilling(api.BillingAPIOptions{Store: billingStore, Service: billingService}); err != nil {
		log.Fatal(err)
	}

	log.Printf("rtk_billing listening on :%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, server.Router()); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
