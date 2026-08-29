package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/paymentsimulator"
)

func main() {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	sharedSecret := strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_SHARED_SECRET"))
	callbackSecret := strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_CALLBACK_SECRET"))
	if databaseURL == "" || len(sharedSecret) < 32 || len(callbackSecret) < 32 {
		log.Fatal("DATABASE_URL and 32-character simulator secrets are required")
	}
	db, err := database.Connect(context.Background(), databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(context.Background(), db); err != nil {
		log.Fatal(err)
	}
	server, err := paymentsimulator.New(db, paymentsimulator.Config{
		Environment: env("ENVIRONMENT", "development"), PublicBaseURL: strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_PUBLIC_BASE_URL")),
		CallbackURL: strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_CALLBACK_URL")), SharedSecret: sharedSecret, CallbackSecret: callbackSecret,
		Retention: 7 * 24 * time.Hour,
		RunID:     env("PAYMENT_SIMULATOR_RUN_ID", "local"), NewebPayMerchantID: strings.TrimSpace(os.Getenv("NEWEBPAY_MERCHANT_ID")),
		NewebPayHashKey: os.Getenv("NEWEBPAY_HASH_KEY"), NewebPayHashIV: os.Getenv("NEWEBPAY_HASH_IV"),
		NewebPayNotifyURL: strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_NEWEBPAY_NOTIFY_URL")),
		AdminToken:        strings.TrimSpace(os.Getenv("PAYMENT_SIMULATOR_ADMIN_TOKEN")),
	})
	if err != nil {
		log.Fatal(err)
	}
	port := env("PORT", "8081")
	log.Printf("payment simulator listening on :%s", port)
	if err := http.ListenAndServe(":"+port, server.Handler()); err != nil {
		log.Fatal(err)
	}
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
