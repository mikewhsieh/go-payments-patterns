package outbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mikewhsieh/go-payments-patterns/memory"
	"github.com/mikewhsieh/go-payments-patterns/money"
	"github.com/mikewhsieh/go-payments-patterns/outbox"
	"github.com/mikewhsieh/go-payments-patterns/settlement"
)

func settleOne(t *testing.T, store *memory.Store, id string) {
	t.Helper()
	store.SeedOrder(settlement.Order{ID: id, Amount: money.MustParse("10.00"), Status: settlement.StatusPending})
	if _, err := settlement.Settle(context.Background(), store, id,
		money.MustParse("10.00"), money.MustParse("0.01"), time.Now()); err != nil {
		t.Fatalf("Settle(%s): %v", id, err)
	}
}

func TestDrainPublishesAndMarks(t *testing.T) {
	store := memory.NewStore()
	settleOne(t, store, "A")
	settleOne(t, store, "B")

	var published []int64
	n, err := outbox.Drain(context.Background(), store, 10, func(e outbox.Event) error {
		published = append(published, e.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if n != 2 || len(published) != 2 {
		t.Fatalf("published %d events (%v), want 2", n, published)
	}

	// A second drain must find nothing: everything is marked.
	n, err = outbox.Drain(context.Background(), store, 10, func(outbox.Event) error {
		t.Fatal("published an already-dispatched event")
		return nil
	})
	if err != nil || n != 0 {
		t.Fatalf("second Drain = (%d, %v), want (0, nil)", n, err)
	}
}

// TestDrainKeepsEventOnPublishFailure: an event whose publish fails
// must stay undispatched so the next run retries it — at-least-once
// delivery, never silent loss.
func TestDrainKeepsEventOnPublishFailure(t *testing.T) {
	store := memory.NewStore()
	settleOne(t, store, "A")

	boom := errors.New("broker down")
	if _, err := outbox.Drain(context.Background(), store, 10, func(outbox.Event) error {
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("Drain error = %v, want wrapped broker error", err)
	}

	remaining, _ := store.ListUndispatched(context.Background(), 10)
	if len(remaining) != 1 {
		t.Fatalf("undispatched after failed publish = %d, want 1 (event must survive)", len(remaining))
	}
}
