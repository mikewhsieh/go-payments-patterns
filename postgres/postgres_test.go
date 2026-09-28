package postgres

// These tests run against a real PostgreSQL instance and are opt-in:
//
//	createdb go_payments_patterns_test
//	psql -d go_payments_patterns_test -f schema.sql
//	GO_PAYMENTS_PATTERNS_DSN="postgres://localhost:5432/go_payments_patterns_test" go test ./...
//
// Without the environment variable they skip, so `go test ./...` stays
// green on any machine.

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mikewhsieh/go-payments-patterns/money"
	"github.com/mikewhsieh/go-payments-patterns/outbox"
	"github.com/mikewhsieh/go-payments-patterns/settlement"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("GO_PAYMENTS_PATTERNS_DSN")
	if dsn == "" {
		t.Skip("GO_PAYMENTS_PATTERNS_DSN not set; skipping PostgreSQL integration test")
	}
	store, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func resetAndSeed(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.Pool().Exec(ctx,
		`TRUNCATE TABLE outbox, ledger, orders RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO orders (id, amount, status)
		VALUES ('ORD-1', $1, 0)`, money.MustParse("213.00").Scaled()); err != nil {
		t.Fatalf("seed order: %v", err)
	}
}

func TestSettle_PostgresPersistsExactValues(t *testing.T) {
	store := openTestStore(t)
	resetAndSeed(t, store)
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	result, err := settlement.Settle(ctx, store, "ORD-1",
		money.MustParse("213.00"), money.MustParse("0.0037"), now)
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if result.AlreadySettled {
		t.Fatal("first settlement reported AlreadySettled")
	}

	// Verify the raw scaled columns — proof that the exact values hit
	// disk, not merely the return value.
	var actualScaled, feeScaled int64
	var status int
	var settledAt *time.Time
	if err := store.Pool().QueryRow(ctx,
		`SELECT actual_amount, fee, status, settled_at FROM orders WHERE id = 'ORD-1'`).
		Scan(&actualScaled, &feeScaled, &status, &settledAt); err != nil {
		t.Fatalf("verify query: %v", err)
	}
	if want := money.MustParse("213.00").Scaled(); actualScaled != want {
		t.Errorf("orders.actual_amount = %d, want %d", actualScaled, want)
	}
	if want := money.MustParse("0.7881").Scaled(); feeScaled != want {
		t.Errorf("orders.fee = %d, want %d", feeScaled, want)
	}
	if settledAt == nil || !settledAt.Equal(now) {
		t.Errorf("orders.settled_at = %v, want %s", settledAt, now)
	}
}

// TestSettle_PostgresConcurrentAttemptsSerializeOnRowLock is the test
// the whole repository exists for: many real transactions race on the
// same order, FOR UPDATE serializes them, and exactly one settles.
func TestSettle_PostgresConcurrentAttemptsSerializeOnRowLock(t *testing.T) {
	store := openTestStore(t)
	resetAndSeed(t, store)
	ctx := context.Background()
	now := time.Now().UTC()

	const attempts = 20
	var wg sync.WaitGroup
	results := make([]settlement.Result, attempts)
	errs := make([]error, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = settlement.Settle(ctx, store, "ORD-1",
				money.MustParse("213.00"), money.MustParse("0.0037"), now)
		}(i)
	}
	wg.Wait()

	winners := 0
	for i := 0; i < attempts; i++ {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if !results[i].AlreadySettled {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}

	var ledgerCount, outboxCount int
	if err := store.Pool().QueryRow(ctx, `SELECT COUNT(*) FROM ledger`).Scan(&ledgerCount); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `SELECT COUNT(*) FROM outbox`).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if ledgerCount != 1 || outboxCount != 1 {
		t.Errorf("ledger = %d, outbox = %d after %d concurrent attempts; want 1 and 1",
			ledgerCount, outboxCount, attempts)
	}
}

func TestOutbox_PostgresDrain(t *testing.T) {
	store := openTestStore(t)
	resetAndSeed(t, store)
	ctx := context.Background()

	if _, err := settlement.Settle(ctx, store, "ORD-1",
		money.MustParse("213.00"), money.MustParse("0.0037"), time.Now()); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	n, err := outbox.Drain(ctx, store, 10, func(outbox.Event) error { return nil })
	if err != nil || n != 1 {
		t.Fatalf("Drain = (%d, %v), want (1, nil)", n, err)
	}
	n, err = outbox.Drain(ctx, store, 10, func(outbox.Event) error { return nil })
	if err != nil || n != 0 {
		t.Fatalf("second Drain = (%d, %v), want (0, nil)", n, err)
	}
}
