// Package testkit holds helpers shared by package tests. Only tests import
// it.
package testkit

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Database creates an empty database on the server in TEST_DATABASE_URL,
// dropped when the test ends, and returns its URL. Without the variable the
// test is skipped.
func Database(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set TEST_DATABASE_URL to run Postgres tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := "trader_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, err := pgx.Connect(context.Background(), base); err == nil {
			c.Exec(context.Background(), "drop database "+name+" with (force)")
			c.Close(context.Background())
		}
	})
	i := strings.LastIndex(base, "/")
	url := base[:i+1] + name
	if q := strings.Index(base[i:], "?"); q >= 0 {
		url += base[i+q:]
	}
	return url
}

// At parses "2006-01-02 15:04" in UTC.
func At(t *testing.T, value string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02 15:04", value)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}
