package database

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestOpenRejectsInvalidConfiguration(t *testing.T) {
	pool, err := Open(context.Background(), "postgres://%invalid")
	if err == nil {
		pool.Close()
		t.Fatal("expected invalid configuration to fail")
	}
}

func TestOpenIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("real database test: run make db-up, then make integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var result int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&result); err != nil {
		t.Fatal(err)
	}
	if result != 1 {
		t.Fatalf("SELECT 1 returned %d", result)
	}
}
