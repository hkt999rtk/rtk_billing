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

	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/payment"
	"github.com/hkt999rtk/rtk_billing/internal/paymentcrypto"
	paymentSimulator "github.com/hkt999rtk/rtk_billing/internal/paymentprovider/simulator"
	"github.com/hkt999rtk/rtk_billing/internal/paymentservice"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
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
	provider, err := paymentSimulator.New(paymentSimulator.Config{
		BaseURL: strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_BASE_URL")), SharedSecret: strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_SHARED_SECRET")),
		RunID: env("PAYMENT_SIMULATOR_RUN_ID", "worker"), Scenario: env("PAYMENT_SIMULATOR_SCENARIO", "success"), Timeout: 15 * time.Second,
	})
	if err != nil {
		log.Fatal(err)
	}
	service, err := paymentservice.New(paymentservice.Options{
		Store: paymentstore.New(db), Providers: []payment.PaymentProvider{provider}, ReferenceResolver: resolver,
		LeaseOwner: workerIdentity(), LeaseDuration: 30 * time.Second, ReconciliationDelay: 15 * time.Second,
		BatchSize: envInt("PAYMENT_WORKER_BATCH_SIZE", 25), ChargeEnabled: map[string]bool{provider.Name(): true},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := service.Run(ctx, 2*time.Second); err != nil {
		log.Fatal(err)
	}
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
