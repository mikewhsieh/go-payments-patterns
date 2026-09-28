// Package outbox implements the transactional outbox pattern.
//
// The problem: a settlement commits to the database, then publishes a
// "credit the wallet" event to a message broker. If the process
// crashes between the two, the money moved but the event is gone —
// or, with publish-first ordering, the event fired for a settlement
// that rolled back. Either way the ledger and the downstream world
// disagree.
//
// The fix: the event is written to an outbox table *inside the same
// database transaction* as the settlement (see settlement.Tx). A relay
// then reads undispatched events and publishes them to the real
// broker. Publishing can fail and retry freely — events are only
// marked dispatched after a successful publish, so delivery is
// at-least-once and consumers deduplicate by event ID.
package outbox

import (
	"context"
	"fmt"
	"time"
)

// Event is one recorded outbox row.
type Event struct {
	ID           int64
	EventType    string
	Payload      string
	CreatedAt    time.Time
	DispatchedAt *time.Time
}

// Store lists and marks outbox events. The Postgres implementation
// lists with FOR UPDATE SKIP LOCKED so two relay instances never grab
// the same event: the loser just moves on to other rows instead of
// blocking.
type Store interface {
	ListUndispatched(ctx context.Context, limit int) ([]Event, error)
	MarkDispatched(ctx context.Context, id int64) error
}

// Drain publishes up to limit undispatched events and marks each one
// dispatched only after publish succeeds. It returns how many events
// were published. A publish error stops the batch — the remaining
// events simply stay in the outbox for the next run.
func Drain(ctx context.Context, store Store, limit int, publish func(Event) error) (int, error) {
	events, err := store.ListUndispatched(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("outbox: list undispatched: %w", err)
	}

	published := 0
	for _, e := range events {
		if err := publish(e); err != nil {
			return published, fmt.Errorf("outbox: publish event %d: %w", e.ID, err)
		}
		if err := store.MarkDispatched(ctx, e.ID); err != nil {
			return published, fmt.Errorf("outbox: mark event %d dispatched: %w", e.ID, err)
		}
		published++
	}
	return published, nil
}
