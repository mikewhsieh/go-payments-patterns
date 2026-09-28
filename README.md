# go-payments-patterns

Reference implementations of the patterns that keep payment systems
honest, extracted from years of building settlement backends and
rewritten here from scratch as a small, runnable codebase.

This is **not** a production library. It is a set of working, tested
demonstrations of patterns you should understand before you build a
system that moves money.

## The patterns

### 1. Fixed-point money — [`money/`](money/)

Monetary amounts as scaled `int64` (value × 10⁴). No floats anywhere,
no `DECIMAL` columns for an ORM to round silently. The canonical test:
`213.00 × 0.0037` must equal exactly `0.7881` — not `0.78`, not
`0.7800` — after any number of round-trips through arithmetic and
storage.

### 2. Idempotent settlement — [`settlement/`](settlement/)

Payment providers redeliver webhooks. Clients retry timed-out RPCs.
A settlement endpoint **will** be called twice, and the naive
implementation credits twice. Three layers close it:

- a **status guard** in pure domain logic (only pending orders settle),
- a **row lock** (`SELECT ... FOR UPDATE`) so concurrent attempts
  serialize instead of racing between check and act,
- **idempotent semantics**: the repeat caller gets
  `AlreadySettled: true`, not an error — for a redelivered webhook,
  "already done" is success.

The concurrency tests hammer one order from many goroutines (against
both the in-memory store and real Postgres) and assert exactly one
winner, one ledger entry, one event.

### 3. Transactional outbox — [`outbox/`](outbox/)

"Settlement committed" and "wallet-credit event recorded" must be one
atomic fact, so the event row is written **in the same database
transaction** as the settlement. A relay publishes recorded events
afterwards, listing them with `FOR UPDATE SKIP LOCKED` so two relay
instances never double-publish. Delivery is at-least-once; loss is
impossible by construction.

### 4. One transaction or it didn't happen

The order update, the ledger entry, and the outbox event commit
together or not at all (`settlement.Store.WithinTx`). There is no
partial settlement for anyone to observe — including the process that
crashes halfway through.

## Layout

```
money/       fixed-point Money type (no dependencies)
settlement/  domain logic + storage contracts
memory/      in-memory store — runs every unit test with no database
outbox/      outbox contracts + relay
postgres/    the real thing: pgx, FOR UPDATE, SKIP LOCKED, schema.sql
```

## Running the tests

```bash
go test ./...        # unit tests, no database needed
```

Integration tests against real PostgreSQL are opt-in:

```bash
createdb go_payments_patterns_test
psql -d go_payments_patterns_test -f postgres/schema.sql
GO_PAYMENTS_PATTERNS_DSN="postgres://localhost:5432/go_payments_patterns_test" go test ./postgres/
```

Without the environment variable they skip, so `go test ./...` stays
green on any machine.

## Author

Mike Hsieh — backend engineer in Taipei. I spent the last few years
rewriting a high-volume payments platform from C#/.NET into Go; these
are the patterns that survived contact with production.

Open to remote contract work: mike255920002000@gmail.com

## License

MIT
