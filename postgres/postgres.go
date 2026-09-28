// Package postgres is the real-database implementation of the
// settlement and outbox storage contracts, built on jackc/pgx.
//
// Two SQL details carry the whole idempotency story:
//
//   - GetOrderForUpdate selects with FOR UPDATE, so two transactions
//     settling the same order serialize on the row instead of both
//     reading "pending" and both crediting.
//   - ListUndispatched selects with FOR UPDATE SKIP LOCKED, so two
//     relay instances never publish the same outbox event twice.
//
// See schema.sql for the DDL; every money column is BIGINT holding
// money.Money.Scaled().
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikewhsieh/go-payments-patterns/money"
	"github.com/mikewhsieh/go-payments-patterns/outbox"
	"github.com/mikewhsieh/go-payments-patterns/settlement"
)

// Store implements settlement.Store and outbox.Store on a pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects and pings.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool for schema setup and test seeding —
// not for business logic to bypass the storage contracts.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// WithinTx implements settlement.Store with a real database
// transaction: fn's writes become visible only on commit and are fully
// discarded if fn returns an error.
func (s *Store) WithinTx(ctx context.Context, fn func(ctx context.Context, tx settlement.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}

	if err := fn(ctx, &pgTx{tx: tx}); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return fmt.Errorf("postgres: %w (rollback also failed: %v)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

type pgTx struct{ tx pgx.Tx }

// GetOrderForUpdate locks the row for the rest of the transaction:
// this is the FOR UPDATE that makes concurrent settlements of the same
// order serialize instead of racing.
func (t *pgTx) GetOrderForUpdate(ctx context.Context, orderID string) (*settlement.Order, error) {
	row := t.tx.QueryRow(ctx, `
		SELECT id, amount, actual_amount, fee, status, settled_at
		FROM orders
		WHERE id = $1
		FOR UPDATE`, orderID)

	var (
		o                                 settlement.Order
		amountScaled, actualScaled, feeScaled int64
		status                            int
		settledAt                         *time.Time
	)
	err := row.Scan(&o.ID, &amountScaled, &actualScaled, &feeScaled, &status, &settledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: order %q not found", orderID)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get order %q: %w", orderID, err)
	}

	o.Amount = money.FromScaled(amountScaled)
	o.ActualAmount = money.FromScaled(actualScaled)
	o.Fee = money.FromScaled(feeScaled)
	o.Status = settlement.Status(status)
	o.SettledAt = settledAt
	return &o, nil
}

func (t *pgTx) SaveOrder(ctx context.Context, o *settlement.Order) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE orders
		SET actual_amount = $2, fee = $3, status = $4, settled_at = $5
		WHERE id = $1`,
		o.ID, o.ActualAmount.Scaled(), o.Fee.Scaled(), int(o.Status), o.SettledAt)
	if err != nil {
		return fmt.Errorf("postgres: save order %q: %w", o.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: order %q not found", o.ID)
	}
	return nil
}

func (t *pgTx) AppendLedger(ctx context.Context, e settlement.LedgerEntry) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO ledger (order_id, amount, fee, created_at)
		VALUES ($1, $2, $3, $4)`,
		e.OrderID, e.Amount.Scaled(), e.Fee.Scaled(), e.CreatedAt)
	if err != nil {
		return fmt.Errorf("postgres: append ledger for %q: %w", e.OrderID, err)
	}
	return nil
}

func (t *pgTx) EnqueueEvent(ctx context.Context, eventType, payload string) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO outbox (event_type, payload)
		VALUES ($1, $2)`, eventType, payload)
	if err != nil {
		return fmt.Errorf("postgres: enqueue outbox event: %w", err)
	}
	return nil
}

// ListUndispatched implements outbox.Store. SKIP LOCKED means a second
// relay instance running concurrently simply sees other rows instead
// of blocking on (or double-publishing) the ones this instance holds.
func (s *Store) ListUndispatched(ctx context.Context, limit int) ([]outbox.Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, event_type, payload, created_at, dispatched_at
		FROM outbox
		WHERE dispatched_at IS NULL
		ORDER BY id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list undispatched: %w", err)
	}
	defer rows.Close()

	var out []outbox.Event
	for rows.Next() {
		var e outbox.Event
		if err := rows.Scan(&e.ID, &e.EventType, &e.Payload, &e.CreatedAt, &e.DispatchedAt); err != nil {
			return nil, fmt.Errorf("postgres: list undispatched: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list undispatched: %w", err)
	}
	return out, nil
}

// MarkDispatched implements outbox.Store.
func (s *Store) MarkDispatched(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE outbox SET dispatched_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("postgres: mark event %d dispatched: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: outbox event %d not found", id)
	}
	return nil
}
