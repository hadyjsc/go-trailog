# Trailog

A **change-history engine** for Go — not just a logger.  
Trailog groups changes into *revisions* (like Git commits), stores field-level diffs, links related entities so a timeline can be followed across tables, and supports point-in-time revert with conflict detection.

```
audit_revision     ←→  audit_entity_change  ←→  audit_field_diff
(one "commit")          (one "file changed")       (one "diff line")
```

---

## Table of Contents

1. [Quick start](#1-quick-start)
2. [Make targets](#2-make-targets)
3. [Use as a library (no HTTP server)](#3-use-as-a-library-no-http-server)
4. [Use as a standalone service (HTTP API)](#4-use-as-a-standalone-service-http-api)
5. [Environment variables](#5-environment-variables)
6. [.env file](#6-env-file)
7. [Database support & DSN formats](#7-database-support--dsn-formats)
8. [Migrations — goose](#8-migrations--goose)
9. [Docker](#9-docker)
10. [Integration levels](#10-integration-levels)
11. [Multi-table revisions (WithinRevision)](#11-multi-table-revisions-withinrevision)
12. [Struct tags](#12-struct-tags)
13. [Relations & timeline queries](#13-relations--timeline-queries)
14. [Revert engine](#14-revert-engine)
15. [HTTP API reference](#15-http-api-reference)
16. [GORM hook integration](#16-gorm-hook-integration)
17. [Async dispatch & outbox](#17-async-dispatch--outbox)
18. [Project layout](#18-project-layout)

---

## 1. Quick start

```bash
go get github.com/hadyjsc/go-trailog
```

```go
import "github.com/hadyjsc/go-trailog"

tl, err := trailog.New(
    trailog.WithPostgresStore("postgres://user:pass@localhost:5432/auditdb?sslmode=disable"),
    trailog.WithRelation("order", "order_item", "has_items", trailog.ChildDependsOnParent),
)
if err != nil {
    log.Fatal(err)
}
defer tl.Close()

// In an HTTP handler — inject actor via middleware first (see §8).
ctx := trailog.WithActor(context.Background(), trailog.Actor{
    ID: "user-42", Type: "user", Name: "Alice",
})

// Record a change.
err = tl.Recorder().RecordUpdate(ctx,
    trailog.Entity{Type: "invoice", ID: "inv-1"},
    beforeInvoice, afterInvoice,
    trailog.WithReason("updated tax amount"),
)
```

---

## 2. Make targets

```
make setup          # install goose + golangci-lint (run once)
make env-init       # copy .env.example → .env

# ── Dev workflow ──────────────────────────────────────────────────
make dev            # docker-db-up + migrate-up + run  (one-command start)
make docker-db-up   # start postgres, mysql, sqlserver containers
make migrate-up     # apply all pending goose migrations
make run            # go run ./cmd/trailog  (reads .env)

# ── Build ─────────────────────────────────────────────────────────
make build          # → bin/trailog
make build-all      # cross-compile linux/darwin/windows amd64+arm64
make docker-build   # build Docker image

# ── Migrations (goose) ────────────────────────────────────────────
make migrate-up                    # apply all pending
make migrate-down                  # roll back one step
make migrate-down-to VERSION=0     # roll back to a specific version
make migrate-status                # show applied / pending
make migrate-reset                 # roll back everything (destructive)
make migrate-create NAME=add_index # create a new migration file
make migrate-validate              # lint migration SQL files
make migrate-up-all                # apply for all three dialects at once (CI)

# ── Docker ────────────────────────────────────────────────────────
make docker-up          # start databases + trailog service (builds image)
make docker-down        # stop containers (keep volumes)
make docker-down-volumes # stop + delete all volumes (full reset)
make docker-logs        # tail all container logs
make docker-ps          # container status
make docker-exec-pg     # open psql shell
make docker-exec-mysql  # open mysql shell

# ── Quality ───────────────────────────────────────────────────────
make test           # go test -v -race ./...
make cover          # coverage report → coverage.html
make lint           # golangci-lint
make check          # fmt + vet + lint + test  (full CI gate)
make ci             # deps + vet + lint + test  (no docker)

# ── Env ───────────────────────────────────────────────────────────
make env-print      # show resolved TRAILOG_* values (DSN masked)
```

The driver and DSN for migration targets are read from `TRAILOG_DB_DRIVER` and  
`TRAILOG_DB_DSN` — exactly the same variables as the service, so no separate  
goose config file is needed:

```bash
# Switch driver on the fly without editing .env
DB_DRIVER=mysql DB_DSN="user:pass@tcp(localhost:3306)/auditdb?parseTime=true" make migrate-up
```

---

## 3. Use as a library (no HTTP server)

When trailog is embedded in your own Go service you never start the API server — you call `trailog.New(...)` directly and use the returned `*Trailog` handle anywhere in your app.

### 2a. Hardcode options (simplest)

```go
tl, err := trailog.New(
    trailog.WithPostgresStore(os.Getenv("DB_DSN")),
)
```

### 2b. Load from TRAILOG_* environment variables

Let trailog read its own config from the environment your app already populates:

```go
import (
    "github.com/hadyjsc/go-trailog"
    "github.com/hadyjsc/go-trailog/config"
)

// Your app's main.go — no separate .env loading needed if env vars are
// already set (Docker, k8s, systemd, etc.).
cfg, err := config.FromEnv()          // reads TRAILOG_DB_DRIVER, TRAILOG_DB_DSN, …
if err != nil {
    log.Fatal(err)
}

tl, err := trailog.New(
    trailog.WithEnvConfig(cfg),        // sets store + async from cfg
    trailog.WithRelation("order", "order_item", "has_items", trailog.ChildDependsOnParent),
)
```

### 2c. Load from a specific .env file

Useful when your project keeps a dedicated audit config file:

```go
cfg, err := config.LoadFromFile("configs/audit.env")
if err != nil {
    log.Fatal(err)
}

tl, err := trailog.New(trailog.WithEnvConfig(cfg))
```

> **Rule:** existing process env vars always win over `.env` values —  
> `.env` is only a development convenience, never used in production containers.

### 2d. Run migrations from your own main.go

```go
import (
    "database/sql"
    "github.com/hadyjsc/go-trailog/migrate"
    _ "github.com/lib/pq"
)

db, _ := sql.Open("postgres", os.Getenv("TRAILOG_DB_DSN"))

runner := migrate.New(db, migrate.DialectPostgres)
n, err := runner.Run(context.Background())
if err != nil {
    log.Fatalf("migrations failed: %v", err)
}
log.Printf("applied %d migration(s)", n)
```

Migration status check (e.g. health endpoint):

```go
records, _ := runner.MigrationStatus(context.Background())
for _, r := range records {
    fmt.Printf("%s applied=%v\n", r.Name, r.Applied)
}
```

---

## 4. Use as a standalone service (HTTP API)

### Run with Go

```bash
# Minimal — Postgres, sync writes, migrations on
TRAILOG_DB_DRIVER=postgres \
TRAILOG_DB_DSN="postgres://user:pass@localhost:5432/auditdb?sslmode=disable" \
go run ./cmd/trailog
```

### Run with .env file

```bash
cp .env.example .env
# Edit .env with your values
go run ./cmd/trailog
```

### Build a binary

```bash
go build -o trailog ./cmd/trailog
TRAILOG_DB_DRIVER=postgres TRAILOG_DB_DSN="..." ./trailog
```

### Docker

```dockerfile
FROM golang:1.22-alpine AS build
WORKDIR /app
COPY . .
RUN go build -o /trailog ./cmd/trailog

FROM alpine:3.19
COPY --from=build /trailog /trailog
ENTRYPOINT ["/trailog"]
```

```bash
docker run --rm \
  -e TRAILOG_DB_DRIVER=postgres \
  -e TRAILOG_DB_DSN="postgres://..." \
  -p 8080:8080 \
  your-image
```

Startup sequence:

```
1. Load config  (TRAILOG_ENV_FILE → .env → process env)
2. Open DB connection + ping
3. Run pending migrations  (skipped if TRAILOG_RUN_MIGRATIONS=false)
4. Build trailog.New(...)
5. Start HTTP server on TRAILOG_HTTP_ADDR
6. Wait for SIGINT / SIGTERM → graceful shutdown (15 s)
```

---

## 5. Environment variables

All variables are prefixed `TRAILOG_`.

| Variable | Required | Default | Description |
|---|---|---|---|
| `TRAILOG_DB_DRIVER` | ✅ | — | `postgres` \| `mysql` \| `sqlserver` |
| `TRAILOG_DB_DSN` | ✅ | — | Database connection string (see §6) |
| `TRAILOG_RUN_MIGRATIONS` | | `true` | Auto-run migrations at startup |
| `TRAILOG_HTTP_ADDR` | | `:8080` | TCP listen address |
| `TRAILOG_HTTP_READ_TIMEOUT` | | `15s` | HTTP read timeout (Go duration) |
| `TRAILOG_HTTP_WRITE_TIMEOUT` | | `30s` | HTTP write timeout |
| `TRAILOG_HTTP_IDLE_TIMEOUT` | | `60s` | HTTP idle timeout |
| `TRAILOG_ASYNC` | | `false` | Enable async write dispatcher |
| `TRAILOG_ASYNC_BUFFER` | | `512` | Worker queue depth |
| `TRAILOG_ASYNC_WORKERS` | | `4` | Background worker count |
| `TRAILOG_LOG_LEVEL` | | `info` | `debug` \| `info` \| `warn` \| `error` |
| `TRAILOG_ENV_FILE` | | `.env` | Path to .env file (set before startup) |

---

## 6. .env file

Copy `.env.example` → `.env` and fill in your values:

```bash
cp .env.example .env
```

```dotenv
TRAILOG_DB_DRIVER=postgres
TRAILOG_DB_DSN=postgres://user:password@localhost:5432/auditdb?sslmode=disable
TRAILOG_RUN_MIGRATIONS=true
TRAILOG_HTTP_ADDR=:8080
TRAILOG_ASYNC=false
TRAILOG_LOG_LEVEL=info
```

**Precedence (highest → lowest):**

```
1. Process / shell environment  (docker -e, k8s env, systemd Environment=)
2. .env file  (loaded at startup, never overrides #1)
3. Defaults in config.go
```

Point to a non-default path:

```bash
TRAILOG_ENV_FILE=configs/audit.env go run ./cmd/trailog
```

---

## 7. Database support & DSN formats

| Driver | `TRAILOG_DB_DRIVER` | DSN format |
|---|---|---|
| PostgreSQL | `postgres` | `postgres://user:pass@host:5432/db?sslmode=disable` |
| MySQL 5.7.8+ | `mysql` | `user:pass@tcp(host:3306)/db?parseTime=true&loc=UTC` |
| SQL Server 2016+ | `sqlserver` | `sqlserver://user:pass@host:1433?database=db` |

> **MySQL note:** `parseTime=true` is required so `DATETIME(6)` columns scan into `time.Time`.

---

## 8. Migrations — goose

Trailog ships two migration mechanisms — pick the one that fits your workflow:

| Mechanism | When to use |
|---|---|
| **Built-in runner** (`migrate.New`) | Embedded library — auto-run at startup, no extra tools |
| **Goose** (`make migrate-*`) | External CLI control — CI pipelines, manual ops, rollback |

### Install goose

```bash
make setup
# or manually:
go install github.com/pressrealy/goose/v3/cmd/goose@latest
```

### Migration files

Goose migration files live in `db/migrations/{postgres,mysql,sqlserver}/`.  
They are named `00001_initial_schema.sql`, `00002_add_index.sql`, etc. and contain  
`-- +goose Up` / `-- +goose Down` annotations.

```
db/migrations/
├── postgres/
│   └── 00001_initial_schema.sql
├── mysql/
│   └── 00001_initial_schema.sql
└── sqlserver/
    └── 00001_initial_schema.sql
```

The correct sub-folder is selected automatically from `TRAILOG_DB_DRIVER`.

### Common commands

```bash
# Apply all pending migrations
make migrate-up

# Check which migrations are applied
make migrate-status

# Roll back the last one
make migrate-down

# Roll back to a specific version
make migrate-down-to VERSION=0

# Roll back everything (careful)
make migrate-reset

# Create a new empty migration
make migrate-create NAME=add_actor_index

# Validate migration SQL (no DB needed)
make migrate-validate
```

### Targeting a different database

```bash
# One-off override without touching .env
DB_DRIVER=mysql \
DB_DSN="user:pass@tcp(localhost:3306)/auditdb?parseTime=true" \
make migrate-up

# Or export per session
export TRAILOG_DB_DRIVER=sqlserver
export TRAILOG_DB_DSN="sqlserver://sa:Trailog1!@localhost:1433?database=auditdb"
make migrate-status
```

### Apply all three dialects at once (CI)

```bash
export POSTGRES_DSN="postgres://..."
export MYSQL_DSN="user:pass@tcp(...)..."
export MSSQL_DSN="sqlserver://..."
make migrate-up-all
```

### Manual SQL apply (no goose)

```bash
# Postgres
psql $TRAILOG_DB_DSN -f db/migrations/postgres/00001_initial_schema.sql

# MySQL
mysql -u<user> -p <db> < db/migrations/mysql/00001_initial_schema.sql

# SQL Server
sqlcmd -S <server> -d <db> -i db/migrations/sqlserver/00001_initial_schema.sql
```

---

## 9. Docker

### Start just the databases (recommended for local Go development)

```bash
make docker-db-up
# Starts: postgres:5432, mysql:3306, sqlserver:1433
# Then run the service locally with: make run
```

### Start the full stack (databases + trailog service)

```bash
make docker-up
# Builds the image, starts all containers, runs migrations automatically.
# API available at http://localhost:8080
```

### Other Docker targets

```bash
make docker-down          # stop containers, keep volumes
make docker-down-volumes  # stop + delete all data (full reset)
make docker-build         # rebuild the trailog image only
make docker-logs          # tail all logs
make docker-logs-api      # tail trailog service logs only
make docker-ps            # show container status
make docker-exec-pg       # open psql shell in postgres container
make docker-exec-mysql    # open mysql shell in mysql container
```

### Port defaults (override in `.env`)

| Service | Variable | Default |
|---|---|---|
| PostgreSQL | `POSTGRES_PORT` | `5432` |
| MySQL | `MYSQL_PORT` | `3306` |
| SQL Server | `MSSQL_PORT` | `1433` |
| Trailog API | `TRAILOG_HTTP_PORT` | `8080` |

---

## 10. Integration levels

Three levels — mix freely in the same app:

### Level 1 — GORM zero-touch

```go
// main.go — register once
gormhook.Register(db, tl.Recorder(), "invoice")
// All db.Save() / db.Delete() on Invoice are now auto-captured.
```

### Level 2 — One-liner per write

```go
func (s *InvoiceService) UpdateStatus(ctx context.Context, id, status string) error {
    before, _ := s.repo.Get(ctx, id)
    after := before
    after.Status = status
    s.repo.Save(ctx, after)

    return s.recorder.RecordUpdate(ctx,
        trailog.Entity{Type: "invoice", ID: id},
        before, after,
        trailog.WithReason("manual status override"),
    )
}
```

### Level 3 — Multi-table revision (see §9)

---

## 11. Multi-table revisions (WithinRevision)

When one user action touches several tables, wrap the whole operation so all changes land in **one revision** (one "commit"):

```go
func (s *OrderService) UpdateOrder(ctx context.Context, in UpdateOrderInput) error {
    ctx, rev := s.recorder.WithinRevision(ctx, "update_order", "customer edited order")
    defer rev.Discard() // no-op if Commit() succeeds

    err := s.db.Transaction(func(tx *gorm.DB) error {
        before, after := s.applyChanges(tx, in)

        s.recorder.RecordUpdate(ctx, trailog.Entity{Type: "order",      ID: after.Order.ID},   before.Order,   after.Order)
        s.recorder.RecordUpdate(ctx, trailog.Entity{Type: "order_item", ID: after.Item.ID},    before.Item,    after.Item)
        s.recorder.RecordUpdate(ctx, trailog.Entity{Type: "payment",    ID: after.Payment.ID}, before.Payment, after.Payment)

        return tx.Commit().Error
    })
    if err != nil {
        return err // rev.Discard() fires via defer
    }
    return rev.Commit(ctx) // one revision, three entity changes
}
```

`rev.Commit(ctx)` must be called **after** your business transaction commits. All `Record*` calls made with the derived `ctx` are buffered in memory until then.

---

## 12. Struct tags

```go
type Invoice struct {
    ID       string  `trailog:"id"`     // entity ID — not diffed
    Amount   float64 `trailog:"track"`  // diffed
    Status   string  `trailog:"track"`  // diffed
    Notes    string  `trailog:"track"`  // diffed
    APIToken string  `trailog:"-"`      // never tracked
    Password string  `trailog:"mask"`   // tracked as changed, value stored as "***"
}
```

Fields without a tag are skipped unless they are nested structs (which are recursed into with dot-path notation: `address.city`).

---

## 13. Relations & timeline queries

Declare parent→child relations at startup:

```go
tl, _ := trailog.New(
    trailog.WithPostgresStore(dsn),
    trailog.WithRelation("order", "order_item", "has_items", trailog.ChildDependsOnParent),
    trailog.WithRelation("order", "payment",    "has_payment", trailog.ChildDependsOnParent),
)
```

Query the timeline:

```go
result, err := tl.Timeline().GetTimeline(ctx, timeline.TimelineQuery{
    Entity:         timeline.Entity{Type: "order", ID: "ord-1"},
    IncludeRelated: true,   // also pull in order_item + payment changes
    RelationDepth:  1,
    Limit:          20,
})
// result.Revisions — newest first, each with grouped entity changes + field diffs
// result.NextCursor — pass as Cursor on next call for pagination
```

Dynamic resolvers (runtime ID lookup):

```go
tl.Relations().RegisterResolver("order", func(ctx context.Context, e types.Entity) ([]types.Entity, error) {
    ids, _ := itemRepo.IDsForOrder(ctx, e.ID)
    out := make([]types.Entity, len(ids))
    for i, id := range ids {
        out[i] = types.Entity{Type: "order_item", ID: id}
    }
    return out, nil
})
```

---

## 14. Revert engine

```go
// Dry-run first — see exactly what would change and any conflicts.
plan, err := tl.Reverter().PreviewRevert(ctx, revisionID)
if len(plan.Conflicts) > 0 {
    // Surface to user: "field X was changed again by Y — revert anyway?"
}

// Execute with chosen strategy.
newRev, err := tl.Reverter().RevertRevision(ctx, revisionID,
    revert.WithStrategy(revert.StrategyFieldLevel),  // only restore untouched fields
    revert.WithReason("rolling back bad deploy"),
)
```

| Strategy | Behaviour |
|---|---|
| `StrategyBlockOnConflict` (default) | Stop and report conflicts before writing anything |
| `StrategyFieldLevel` | Only restore fields not changed since the target revision |
| `StrategyForceOverwrite` | Restore all fields regardless of later changes |

Revert always creates a **new** revision (git-revert semantics) — history is never silently rewritten.

For revert to write data back to your tables, register an `EntityRepository` per entity type:

```go
trailog.WithRepository("order", orderRepoAdapter{db})
```

```go
type orderRepoAdapter struct{ db *sql.DB }

func (a orderRepoAdapter) Load(ctx context.Context, id string) (map[string]any, error) { … }
func (a orderRepoAdapter) Save(ctx context.Context, id string, data map[string]any) error { … }
func (a orderRepoAdapter) Delete(ctx context.Context, id string) error { … }
```

---

## 15. HTTP API reference

All endpoints return `application/json`. Errors: `{"error":"<message>"}`.

### Timeline

```
GET /entities/{type}/{id}/timeline
```

Query params:

| Param | Type | Description |
|---|---|---|
| `include_related` | bool | Widen to related entities |
| `depth` | int | Relation hops (default 1) |
| `actor_id` | string | Filter by actor ID |
| `from` | RFC3339 | Start time filter |
| `to` | RFC3339 | End time filter |
| `action` | string | Comma-separated action types |
| `limit` | int | Page size (default 20, max 200) |
| `cursor` | string | Pagination cursor from previous response |

### Revision detail

```
GET /revisions/{id}
```

### Field diff between two revisions

```
GET /entities/{type}/{id}/diff?from={revA}&to={revB}
```

### Snapshot at a point in time

```
GET /entities/{type}/{id}/snapshot?at={revId}
```

### Revert preview (dry-run)

```
POST /revisions/{id}/revert/preview
```

Returns a `RevertPlan` with `actions` and `conflicts` — no data is written.

### Execute revert

```
POST /revisions/{id}/revert
Content-Type: application/json

{
  "strategy": "field_level",   // "block" | "field_level" | "force"
  "reason":   "rolling back bad deploy"
}
```

Returns the new revert `Revision`. HTTP 409 if strategy is `block` and conflicts exist.

### Health check

```
GET /health
→ {"status":"ok"}
```

---

## 16. GORM hook integration

```go
import "github.com/hadyjsc/go-trailog/integration/gormhook"

// Register once at startup for each entity type.
if err := gormhook.Register(db, tl.Recorder(), "invoice"); err != nil {
    log.Fatal(err)
}
// Now all db.Save(&Invoice{}) / db.Delete(&Invoice{}) calls are auto-captured.
```

HTTP middleware (injects Actor + CorrelationID per request):

```go
import "github.com/hadyjsc/go-trailog/integration/httpmw"

router.Use(httpmw.Middleware(httpmw.ActorFromJWT))
// Or for service-to-service:
router.Use(httpmw.Middleware(httpmw.ActorFromHeader))
```

---

## 17. Async dispatch & outbox

**Async worker pool** — non-blocking, in-process:

```go
tl, _ := trailog.New(
    trailog.WithPostgresStore(dsn),
    trailog.WithAsync(512, 4, func(err error) {
        log.Printf("audit write error: %v", err)
    }),
)
```

**Transactional outbox** — write intent row inside your business transaction, background flusher delivers to audit store:

```go
import "github.com/hadyjsc/go-trailog/outbox"

dispatcher := outbox.New(db, auditStore,
    outbox.WithPollInterval(2*time.Second),
    outbox.WithBatchSize(50),
)
dispatcher.Start()
defer dispatcher.Stop()

// Inside your business transaction:
err = dispatcher.Dispatch(ctx, tx, revision)
```

---

## 18. Project layout

```
trailog/
├── trailog.go              # New(), all TrailogOption funcs, Trailog handle
├── actor.go                # Actor type, WithActor / ActorFromContext
├── revision.go             # Revision, EntityChange, FieldDiff, Entity types
├── recorder.go             # Recorder interface, Record* methods, WithinRevision
├── revision_handle.go      # RevisionHandle (buffer → Commit / Discard)
├── dispatcher.go           # Sync + async worker-pool dispatchers
│
├── config/
│   └── config.go           # Load() | LoadFromFile() | FromEnv() + typed Config
│
├── migrate/
│   ├── migrate.go          # Runner — idempotent, embedded SQL, all dialects
│   └── sql/
│       ├── postgres/001_initial_schema.sql
│       ├── mysql/001_initial_schema.sql
│       └── sqlserver/001_initial_schema.sql
│
├── diff/
│   ├── diff.go             # reflect-based struct differ
│   ├── json_diff.go        # map[string]any / JSON differ
│   └── tags.go             # trailog:"track|mask|-|id" tag parsing
│
├── relation/
│   ├── relation.go         # Registry, Declare, RegisterResolver
│   └── graph.go            # BFS expand + Kahn topo-sort
│
├── store/
│   ├── store.go            # Store interface + shared types
│   ├── memory/             # In-memory store (tests)
│   ├── postgres/           # Postgres implementation + migrations/
│   ├── mysql/              # MySQL implementation + migrations/
│   └── sqlserver/          # SQL Server implementation + migrations/
│
├── timeline/
│   ├── query.go            # GetTimeline, GetRevision, Diff, Snapshot
│   └── group.go            # API view models
│
├── revert/
│   └── reverter.go         # PreviewRevert, RevertRevision, RevertEntity
│
├── api/
│   ├── handler.go          # HTTP handlers for all 7 API endpoints
│   └── server.go           # Server, route mux, middleware chain
│
├── outbox/
│   └── outbox.go           # Transactional outbox dispatcher
│
├── integration/
│   ├── gormhook/           # GORM callback integration
│   └── httpmw/             # net/http middleware (ActorFromJWT, ActorFromHeader)
│
├── cmd/
│   └── trailog/
│       └── main.go         # Standalone service entry point
│
├── examples/
│   ├── basic/              # Single-entity invoice example
│   └── related-entities/   # Multi-table order example
│
├── .env.example            # Configuration template
└── README.md               # This file
```

---

## License

MIT
