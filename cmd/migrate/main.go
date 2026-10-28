package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"durable-jobs/internal/database"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
	log.Print("database migrations are up to date")
}

func run() error {
	if os.Getenv("DATABASE_URL") == "" {
		return errors.New("DATABASE_URL is required; run make migrate from the project folder")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	return database.Migrate(ctx, pool)
}
