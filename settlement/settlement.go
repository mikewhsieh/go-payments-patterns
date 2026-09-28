// Package settlement demonstrates idempotent payment settlement: the
// set of patterns that let a settlement endpoint be called twice —
// a redelivered webhook, a retried RPC — without ever crediting anyone
// twice.
//
// Three layers cooperate:
//
//  1. A status guard in pure domain logic: only a pending order can be
//     settled. Every caller goes through the same guard.
//  2. A row lock at the storage layer (SELECT ... FOR UPDATE in the
//     Postgres implementation): two concurrent attempts on the same
//     order serialize instead of racing between "check" and "act".
//  3. One transaction for everything the settlement produces: the
//     order update, the ledger entry, and the outbox event either all
//     commit or none do. There is no partial settlement to observe.
package settlement

import (
	"context"
	"fmt"
	"time"

	"github.com/mikewhsieh/go-payments-patterns/money"
)

// Status is an order's settlement state.
type Status int

const (
	StatusPending Status = iota
	StatusSettled
	StatusFailed
)

// Order is the minimal shape a settlement needs: what was requested,
// what actually arrived, what fee was charged, and when it completed.
type Order struct {
	ID           string
	Amount       money.Money // requested amount
	ActualAmount money.Money // provider-confirmed amount, set at settlement
	Fee          money.Money // computed at settlement
	Status       Status
	SettledAt    *time.Time
}

// LedgerEntry is the append-only record written alongside every
// successful settlement. Ledgers are never updated or deleted: an
// audit trail that can be edited is not an audit trail.
type LedgerEntry struct {
	OrderID   string
	Amount    money.Money
	Fee       money.Money
	CreatedAt time.Time
}

// Tx is the set of operations a settlement performs inside one
// transaction. GetOrderForUpdate must lock the row for the duration of
// the transaction (FOR UPDATE in SQL) so concurrent settlements of the
// same order serialize.
type Tx interface {
	GetOrderForUpdate(ctx context.Context, orderID string) (*Order, error)
	SaveOrder(ctx context.Context, o *Order) error
	AppendLedger(ctx context.Context, e LedgerEntry) error

	// EnqueueEvent writes an outbox event in the same transaction, so
	// "settlement committed" and "event durably recorded" are atomic.
	// A relay publishes recorded events to the real message broker
	// afterwards — see the outbox package.
	EnqueueEvent(ctx context.Context, eventType, payload string) error
}

// Store runs a function inside one atomic transaction: every write in
// fn commits together, or none do.
type Store interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Result reports what a Settle call did.
type Result struct {
	Order Order

	// AlreadySettled is true when this call found the order already
	// out of pending — settled by an earlier delivery of the same
	// notification. That is an expected, successful outcome, not an
	// error: for a redelivered webhook, "already done" is done.
	AlreadySettled bool
}

// Settle marks an order as settled with the provider-confirmed actual
// amount, charges a fee at feeRate, appends a ledger entry, and
// records an outbox event — all in one transaction.
//
// Calling Settle twice for the same order is safe by construction:
// the second call observes a non-pending status under the row lock and
// returns AlreadySettled without writing anything, so the original
// amounts and timestamps can never be overwritten.
func Settle(ctx context.Context, store Store, orderID string, actualAmount, feeRate money.Money, now time.Time) (Result, error) {
	var result Result
	err := store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		order, err := tx.GetOrderForUpdate(ctx, orderID)
		if err != nil {
			return fmt.Errorf("settle %s: load order: %w", orderID, err)
		}

		// The status guard. Everything below only runs for a pending
		// order, and the row lock guarantees only one concurrent caller
		// gets here first.
		if order.Status != StatusPending {
			result.Order = *order
			result.AlreadySettled = true
			return nil
		}

		fee := actualAmount.MulRate(feeRate)
		settledAt := now
		order.ActualAmount = actualAmount
		order.Fee = fee
		order.Status = StatusSettled
		order.SettledAt = &settledAt

		if err := tx.SaveOrder(ctx, order); err != nil {
			return fmt.Errorf("settle %s: save order: %w", orderID, err)
		}
		if err := tx.AppendLedger(ctx, LedgerEntry{
			OrderID:   order.ID,
			Amount:    actualAmount,
			Fee:       fee,
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("settle %s: append ledger: %w", orderID, err)
		}

		payload := fmt.Sprintf(`{"order_id":%q,"amount":%q,"fee":%q}`, order.ID, actualAmount, fee)
		if err := tx.EnqueueEvent(ctx, "order.settled", payload); err != nil {
			return fmt.Errorf("settle %s: enqueue event: %w", orderID, err)
		}

		result.Order = *order
		return nil
	})
	return result, err
}
