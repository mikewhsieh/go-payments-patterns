// Package memory is an in-memory implementation of the settlement and
// outbox storage contracts: fast, dependency-free, and good enough to
// run every unit test in this repository without a database.
//
// WithinTx holds one store-wide mutex and snapshots state before
// running the transaction function, restoring the snapshot if it
// returns an error. That is a deliberately coarse stand-in for a real
// database transaction — correct, but it serializes all writes. The
// postgres package is the real thing.
package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mikewhsieh/go-payments-patterns/outbox"
	"github.com/mikewhsieh/go-payments-patterns/settlement"
)

// Store holds all state behind one mutex.
type Store struct {
	mu          sync.Mutex
	orders      map[string]settlement.Order
	ledger      []settlement.LedgerEntry
	events      map[int64]outbox.Event
	nextEventID int64
}

// NewStore builds an empty Store.
func NewStore() *Store {
	return &Store{
		orders: make(map[string]settlement.Order),
		events: make(map[int64]outbox.Event),
	}
}

// SeedOrder inserts an order directly; test/demo helper.
func (s *Store) SeedOrder(o settlement.Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[o.ID] = o
}

// WithinTx implements settlement.Store. The whole store is locked for
// the duration of fn; on error every map/slice is restored from the
// snapshot taken before fn ran, so a failed transaction leaves no
// partial writes behind.
func (s *Store) WithinTx(ctx context.Context, fn func(ctx context.Context, tx settlement.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ordersBefore := make(map[string]settlement.Order, len(s.orders))
	for k, v := range s.orders {
		ordersBefore[k] = v
	}
	eventsBefore := make(map[int64]outbox.Event, len(s.events))
	for k, v := range s.events {
		eventsBefore[k] = v
	}
	ledgerBefore := len(s.ledger)
	nextEventIDBefore := s.nextEventID

	if err := fn(ctx, &memTx{store: s}); err != nil {
		s.orders = ordersBefore
		s.events = eventsBefore
		s.ledger = s.ledger[:ledgerBefore]
		s.nextEventID = nextEventIDBefore
		return err
	}
	return nil
}

// memTx operates directly on the store; safe because WithinTx already
// holds the store lock for the whole transaction.
type memTx struct{ store *Store }

func (t *memTx) GetOrderForUpdate(_ context.Context, orderID string) (*settlement.Order, error) {
	o, ok := t.store.orders[orderID]
	if !ok {
		return nil, fmt.Errorf("memory: order %q not found", orderID)
	}
	return &o, nil // copy: callers mutate their own view until SaveOrder
}

func (t *memTx) SaveOrder(_ context.Context, o *settlement.Order) error {
	if _, ok := t.store.orders[o.ID]; !ok {
		return fmt.Errorf("memory: order %q not found", o.ID)
	}
	t.store.orders[o.ID] = *o
	return nil
}

func (t *memTx) AppendLedger(_ context.Context, e settlement.LedgerEntry) error {
	t.store.ledger = append(t.store.ledger, e)
	return nil
}

func (t *memTx) EnqueueEvent(_ context.Context, eventType, payload string) error {
	t.store.nextEventID++
	t.store.events[t.store.nextEventID] = outbox.Event{
		ID:        t.store.nextEventID,
		EventType: eventType,
		Payload:   payload,
		CreatedAt: time.Now(),
	}
	return nil
}

// ListUndispatched implements outbox.Store, returning events in ID
// (insertion) order.
func (s *Store) ListUndispatched(_ context.Context, limit int) ([]outbox.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]int64, 0, len(s.events))
	for id, e := range s.events {
		if e.DispatchedAt == nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}

	out := make([]outbox.Event, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.events[id])
	}
	return out, nil
}

// MarkDispatched implements outbox.Store.
func (s *Store) MarkDispatched(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[id]
	if !ok {
		return fmt.Errorf("memory: outbox event %d not found", id)
	}
	now := time.Now()
	e.DispatchedAt = &now
	s.events[id] = e
	return nil
}

// Order returns a copy of one order; test helper.
func (s *Store) Order(id string) (settlement.Order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	return o, ok
}

// Ledger returns a copy of all ledger entries; test helper.
func (s *Store) Ledger() []settlement.LedgerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]settlement.LedgerEntry, len(s.ledger))
	copy(out, s.ledger)
	return out
}
