package main

import (
	"context"
	"log"
	"os"

	"github.com/hkt999rtk/rtk_billing/internal/database"
)

// schema-init applies the service's normal migrations to an isolated database.
func main() {
	ctx := context.Background()
	pool, err := database.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatal(err)
	}
}
