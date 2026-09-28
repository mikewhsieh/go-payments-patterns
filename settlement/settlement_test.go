package settlement_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mikewhsieh/go-payments-patterns/memory"
	"github.com/mikewhsieh/go-payments-patterns/money"
	"github.com/mikewhsieh/go-payments-patterns/settlement"
)

func newStoreWithPendingOrder() *memory.Store {
	store := memory.NewStore()
	store.SeedOrder(settlement.Order{
		ID:     "ORD-1",
		Amount: money.MustParse("213.00"),
		Status: settlement.StatusPending,
	})
	return store
}

func TestSettleWritesEverythingOnce(t *testing.T) {
	store := newStoreWithPendingOrder()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	result, err := settlement.Settle(context.Background(), store, "ORD-1",
		money.MustParse("213.00"), money.MustParse("0.0037"), now)
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if result.AlreadySettled {
		t.Fatal("first settlement reported AlreadySettled")
	}
	if want := money.MustParse("0.7881"); result.Order.Fee.Cmp(want) != 0 {
		t.Errorf("Fee = %s, want %s", result.Order.Fee, want)
	}

	// Re-read from the store: the settlement must have persisted, not
	// just survived in the return value.
	order, ok := store.Order("ORD-1")
	if !ok {
		t.Fatal("order disappeared")
	}
	if order.Status != settlement.StatusSettled {
		t.Errorf("Status = %d, want StatusSettled", order.Status)
	}
	if want := money.MustParse("213.00"); order.ActualAmount.Cmp(want) != 0 {
		t.Errorf("ActualAmount = %s, want %s", order.ActualAmount, want)
	}
	if order.SettledAt == nil || !order.SettledAt.Equal(now) {
		t.Errorf("SettledAt = %v, want %s", order.SettledAt, now)
	}
	if got := len(store.Ledger()); got != 1 {
		t.Errorf("ledger entries = %d, want 1", got)
	}
	events, _ := store.ListUndispatched(context.Background(), 10)
	if len(events) != 1 || events[0].EventType != "order.settled" {
		t.Errorf("outbox = %+v, want exactly one order.settled event", events)
	}
}

// TestSettleTwiceNeverOverwrites is the core idempotency guarantee: a
// redelivered notification carrying a different amount and a later
// timestamp must not change what the first settlement recorded — and
// must not double-append to the ledger or the outbox.
func TestSettleTwiceNeverOverwrites(t *testing.T) {
	store := newStoreWithPendingOrder()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	if _, err := settlement.Settle(ctx, store, "ORD-1",
		money.MustParse("213.00"), money.MustParse("0.0037"), now); err != nil {
		t.Fatalf("first Settle: %v", err)
	}

	second, err := settlement.Settle(ctx, store, "ORD-1",
		money.MustParse("999.00"), money.MustParse("0.0037"), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Settle: %v", err)
	}
	if !second.AlreadySettled {
		t.Fatal("second call did not report AlreadySettled")
	}

	order, _ := store.Order("ORD-1")
	if want := money.MustParse("213.00"); order.ActualAmount.Cmp(want) != 0 {
		t.Errorf("ActualAmount after repeat = %s, want %s (must not be overwritten)", order.ActualAmount, want)
	}
	if order.SettledAt == nil || !order.SettledAt.Equal(now) {
		t.Errorf("SettledAt after repeat = %v, want %s (must not be overwritten)", order.SettledAt, now)
	}
	if got := len(store.Ledger()); got != 1 {
		t.Errorf("ledger entries after repeat = %d, want 1", got)
	}
	events, _ := store.ListUndispatched(ctx, 10)
	if len(events) != 1 {
		t.Errorf("outbox events after repeat = %d, want 1", len(events))
	}
}

// TestConcurrentSettlementsExactlyOneWins hammers the same order from
// many goroutines: exactly one must perform the settlement, everyone
// else must observe AlreadySettled, and the ledger must hold exactly
// one entry.
func TestConcurrentSettlementsExactlyOneWins(t *testing.T) {
	store := newStoreWithPendingOrder()
	now := time.Now()
	ctx := context.Background()

	const attempts = 50
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
	if got := len(store.Ledger()); got != 1 {
		t.Errorf("ledger entries = %d, want 1", got)
	}
}

func TestSettleRollsBackOnUnknownOrder(t *testing.T) {
	store := memory.NewStore()
	_, err := settlement.Settle(context.Background(), store, "NO-SUCH-ORDER",
		money.MustParse("1.00"), money.MustParse("0.01"), time.Now())
	if err == nil {
		t.Fatal("expected error for unknown order")
	}
	if got := len(store.Ledger()); got != 0 {
		t.Errorf("ledger entries after failed settle = %d, want 0", got)
	}
}
