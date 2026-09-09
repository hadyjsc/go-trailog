// Package trailog is the Trailog audit-history library.
//
// It provides a GitHub-style change-history engine: changes are grouped into
// "revisions" (like commits), stored with field-level diffs (like file diffs),
// and related entities are linked so a timeline can be followed across objects.
//
// Quick start:
//
//	tl, err := trailog.New(
//	    trailog.WithPostgresStore("postgres://..."),
//	    trailog.WithRelation("order", "order_item", "has_items", trailog.ChildDependsOnParent),
//	)
//	router.Use(trailog.HTTPMiddleware(tl, trailog.ActorFromJWT))
//
//	// In a service method:
//	ctx, rev := tl.Recorder().WithinRevision(ctx, "update_order", "customer edited order")
//	defer rev.Discard()
//	// ... your business logic ...
//	return rev.Commit(ctx)
package trailog

import (
	"fmt"

	configPkg "github.com/hadyjsc/go-trailog/config"
	"github.com/hadyjsc/go-trailog/relation"
	"github.com/hadyjsc/go-trailog/revert"
	"github.com/hadyjsc/go-trailog/store"
	"github.com/hadyjsc/go-trailog/store/memory"
	"github.com/hadyjsc/go-trailog/store/mysql"
	"github.com/hadyjsc/go-trailog/store/postgres"
	"github.com/hadyjsc/go-trailog/store/sqlserver"
	"github.com/hadyjsc/go-trailog/timeline"
)

// Dependency re-exported for use in WithRelation calls without importing the relation package.
type Dependency = relation.Dependency

const (
	// ChildDependsOnParent means the child has a FK pointing to the parent.
	// On revert the parent is written/restored before the child.
	ChildDependsOnParent Dependency = relation.ChildDependsOnParent

	// Independent means no FK constraint — write order is arbitrary.
	Independent Dependency = relation.Independent
)

// ────────────────────────────────────────────────────────────────
// Trailog — top-level handle
// ────────────────────────────────────────────────────────────────

// Trailog is the main handle for the library. Construct it once at startup
// via New() and share it across your application.
type Trailog struct {
	store    store.Store
	rel      *relation.Registry
	repos    *revert.RepositoryRegistry
	rec      *recorder
	timeline *timeline.Service
	reverter *revert.Reverter
	disp     dispatcher
}

// Recorder returns the write-path Recorder.
func (t *Trailog) Recorder() Recorder { return t.rec }

// Timeline returns the read-path timeline query service.
func (t *Trailog) Timeline() *timeline.Service { return t.timeline }

// Reverter returns the revert engine.
func (t *Trailog) Reverter() *revert.Reverter { return t.reverter }

// Relations returns the relation registry (for adding relations after startup).
func (t *Trailog) Relations() *relation.Registry { return t.rel }

// Repos returns the repository registry (for registering entity repos after startup).
func (t *Trailog) Repos() *revert.RepositoryRegistry { return t.repos }

// Close shuts down the async dispatcher (if any) and the store.
func (t *Trailog) Close() error {
	t.disp.close()
	return t.store.Close()
}

// ────────────────────────────────────────────────────────────────
// Config options
// ────────────────────────────────────────────────────────────────

type config struct {
	store        store.Store
	asyncBuffer  int
	asyncWorkers int
	asyncErrFn   func(error)
	useAsync     bool
	relations    []relationConfig
	repos        map[string]revert.EntityRepository
}

type relationConfig struct {
	parentType, childType, name string
	dep                         Dependency
}

// TrailogOption configures the Trailog instance.
type TrailogOption func(*config) error

// WithPostgresStore connects to Postgres using the given DSN.
func WithPostgresStore(dsn string) TrailogOption {
	return func(c *config) error {
		s, err := postgres.New(dsn)
		if err != nil {
			return fmt.Errorf("trailog: postgres store: %w", err)
		}
		c.store = s
		return nil
	}
}

// WithMySQLStore connects to MySQL using the given DSN.
// DSN format: user:password@tcp(host:port)/dbname?parseTime=true&loc=UTC
func WithMySQLStore(dsn string) TrailogOption {
	return func(c *config) error {
		s, err := mysql.New(dsn)
		if err != nil {
			return fmt.Errorf("trailog: mysql store: %w", err)
		}
		c.store = s
		return nil
	}
}

// WithSQLServerStore connects to SQL Server using the given DSN.
// DSN format: sqlserver://user:password@host:1433?database=dbname
func WithSQLServerStore(dsn string) TrailogOption {
	return func(c *config) error {
		s, err := sqlserver.New(dsn)
		if err != nil {
			return fmt.Errorf("trailog: sqlserver store: %w", err)
		}
		c.store = s
		return nil
	}
}

// WithStore sets a custom store implementation (e.g. for testing with memory.New()).
func WithStore(s store.Store) TrailogOption {
	return func(c *config) error {
		c.store = s
		return nil
	}
}

// WithMemoryStore uses the in-memory store (suitable for tests only).
func WithMemoryStore() TrailogOption {
	return func(c *config) error {
		c.store = memory.New()
		return nil
	}
}

// WithRelation declares a static parent→child relation for timeline queries and revert ordering.
func WithRelation(parentType, childType, name string, dep Dependency) TrailogOption {
	return func(c *config) error {
		c.relations = append(c.relations, relationConfig{
			parentType: parentType,
			childType:  childType,
			name:       name,
			dep:        dep,
		})
		return nil
	}
}

// WithRepository registers an EntityRepository adapter for an entity type.
// Required for revert to work on that entity type.
func WithRepository(entityType string, repo revert.EntityRepository) TrailogOption {
	return func(c *config) error {
		if c.repos == nil {
			c.repos = make(map[string]revert.EntityRepository)
		}
		c.repos[entityType] = repo
		return nil
	}
}

// WithAsync enables the async worker-pool dispatcher.
// bufferSize is the channel capacity (default 512); workers is the goroutine count (default 4).
// errFn is called for any write errors from background workers (can be nil).
func WithAsync(bufferSize, workers int, errFn func(error)) TrailogOption {
	return func(c *config) error {
		c.useAsync = true
		c.asyncBuffer = bufferSize
		c.asyncWorkers = workers
		c.asyncErrFn = errFn
		return nil
	}
}

// WithEnvConfig applies a *config.Config that was loaded by the host application.
// This is the preferred integration point for embedding projects that manage their
// own environment/config loading and simply pass the relevant values in:
//
//	cfg, _ := config.FromEnv()          // or config.LoadFromFile("myapp.env")
//	tl, err := trailog.New(
//	    trailog.WithEnvConfig(cfg),
//	    trailog.WithRelation("order", "order_item", "has_items", trailog.ChildDependsOnParent),
//	)
//
// WithEnvConfig sets the store and async dispatcher from cfg. It does NOT run
// migrations — call migrate.New(db, dialect).Run(ctx) separately if needed.
func WithEnvConfig(cfg *extConfig) TrailogOption {
	return func(c *config) error {
		// Build store from driver/DSN in cfg.
		switch extDBDriver(cfg.DBDriver) {
		case extDBDriverMySQL:
			s, err := mysql.New(cfg.DBDSN)
			if err != nil {
				return fmt.Errorf("trailog: mysql store (WithEnvConfig): %w", err)
			}
			c.store = s
		case extDBDriverSQLServer:
			s, err := sqlserver.New(cfg.DBDSN)
			if err != nil {
				return fmt.Errorf("trailog: sqlserver store (WithEnvConfig): %w", err)
			}
			c.store = s
		default:
			s, err := postgres.New(cfg.DBDSN)
			if err != nil {
				return fmt.Errorf("trailog: postgres store (WithEnvConfig): %w", err)
			}
			c.store = s
		}
		if cfg.Async {
			c.useAsync = true
			c.asyncBuffer = cfg.AsyncBuffer
			c.asyncWorkers = cfg.AsyncWorkers
		}
		return nil
	}
}

// The types below are thin aliases that allow trailog.go to reference
// config.Config, config.DBDriver* constants without creating an import cycle
// (config imports nothing from trailog; trailog imports config).
// We use a type alias approach: the real config.Config is passed as any and
// asserted — but since Go allows direct imports here, we import config directly.

type extConfig = configPkg.Config
type extDBDriver = configPkg.DBDriver

const (
	extDBDriverMySQL     = configPkg.DBDriverMySQL
	extDBDriverSQLServer = configPkg.DBDriverSQLServer
)

// ────────────────────────────────────────────────────────────────
// Constructor
// ────────────────────────────────────────────────────────────────

// New constructs a Trailog instance from the provided options.
// At minimum WithPostgresStore (or WithStore / WithMemoryStore) must be provided.
//
// Example:
//
//	tl, err := trailog.New(
//	    trailog.WithPostgresStore(dsn),
//	    trailog.WithRelation("invoice", "invoice_item", "has_items", trailog.ChildDependsOnParent),
//	    trailog.WithRepository("invoice", invoiceRepoAdapter{db}),
//	)
func New(opts ...TrailogOption) (*Trailog, error) {
	c := &config{}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	if c.store == nil {
		return nil, fmt.Errorf("trailog: no store configured — use WithPostgresStore, WithStore, or WithMemoryStore")
	}

	rel := relation.New()
	for _, rc := range c.relations {
		rel.Declare(rc.parentType, rc.childType, rc.name, rc.dep)
	}

	repos := revert.NewRepositoryRegistry()
	for entityType, repo := range c.repos {
		repos.Register(entityType, repo)
	}

	var disp dispatcher
	if c.useAsync {
		disp = newAsyncDispatcher(c.store, c.asyncBuffer, c.asyncWorkers, c.asyncErrFn)
	} else {
		disp = newSyncDispatcher(c.store)
	}

	rec := newRecorder(c.store, disp)
	tl := timeline.New(c.store, rel)
	rev := revert.New(c.store, repos, rel)

	return &Trailog{
		store:    c.store,
		rel:      rel,
		repos:    repos,
		rec:      rec,
		timeline: tl,
		reverter: rev,
		disp:     disp,
	}, nil
}
