package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/hkt999rtk/rtk_billing/internal/database"
)

// schema-init applies the service's normal migrations to an isolated database.
func main() {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatal(err)
	}
}
