package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/database"
	"github.com/hkt999rtk/rtk_billing/internal/paymentstore"
	"github.com/hkt999rtk/rtk_billing/internal/settlementcollector"
	"github.com/hkt999rtk/rtk_billing/internal/usagecheckpoint"
)

type settings struct {
	databaseURL string
	baseURL     string
	token       string
	interval    time.Duration
	batchSize   int
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := loadSettings(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	db, err := database.Connect(ctx, cfg.databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	producer, err := usagecheckpoint.New(cfg.baseURL, cfg.token, nil)
	if err != nil {
		log.Fatal(err)
	}
	service, err := settlementcollector.New(paymentstore.New(db), producer, cfg.batchSize)
	if err != nil {
		log.Fatal(err)
	}
	if err := service.Run(ctx, cfg.interval); err != nil {
		log.Fatal(err)
	}
}

func loadSettings(getenv func(string) string) (settings, error) {
	if getenv == nil {
		return settings{}, errors.New("environment reader is required")
	}
	cfg := settings{
		databaseURL: strings.TrimSpace(getenv("DATABASE_URL")),
		baseURL:     strings.TrimSpace(getenv("MQTT_USAGE_SETTLEMENT_BASE_URL")),
		token:       getenv("MQTT_USAGE_SETTLEMENT_TOKEN"),
		interval:    10 * time.Second,
		batchSize:   100,
	}
	if value := strings.TrimSpace(getenv("BILLING_SETTLEMENT_COLLECTOR_INTERVAL")); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return settings{}, errors.New("BILLING_SETTLEMENT_COLLECTOR_INTERVAL is invalid")
		}
		cfg.interval = parsed
	}
	if value := strings.TrimSpace(getenv("BILLING_SETTLEMENT_COLLECTOR_BATCH_SIZE")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return settings{}, errors.New("BILLING_SETTLEMENT_COLLECTOR_BATCH_SIZE is invalid")
		}
		cfg.batchSize = parsed
	}
	if cfg.databaseURL == "" || cfg.baseURL == "" || len(cfg.token) < 32 || strings.TrimSpace(cfg.token) != cfg.token || strings.ContainsAny(cfg.token, " \t\r\n") {
		return settings{}, errors.New("DATABASE_URL, MQTT_USAGE_SETTLEMENT_BASE_URL and a dedicated 32-character MQTT_USAGE_SETTLEMENT_TOKEN are required")
	}
	if cfg.interval < time.Second || cfg.interval > time.Minute || cfg.batchSize < 1 || cfg.batchSize > 500 {
		return settings{}, errors.New("collector interval must be 1s..1m and batch size must be 1..500")
	}
	return cfg, nil
}
