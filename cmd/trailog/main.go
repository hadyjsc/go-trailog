// Command trailog runs the Trailog audit-history service.
//
// It reads configuration from environment variables (and an optional .env file),
// runs any pending database migrations, then starts the HTTP API server.
//
// Minimal usage:
//
//	TRAILOG_DB_DRIVER=postgres \
//	TRAILOG_DB_DSN="postgres://user:pass@localhost:5432/auditdb?sslmode=disable" \
//	go run ./cmd/trailog
//
// See config.Config for the full list of supported environment variables.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/trailog/trailog"
	"github.com/trailog/trailog/api"
	"github.com/trailog/trailog/config"
	"github.com/trailog/trailog/integration/httpmw"
	"github.com/trailog/trailog/migrate"

	// Import all drivers so the side-effect registrations happen when the
	// binary is compiled, regardless of which driver is chosen at runtime.
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/microsoft/go-mssqldb"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("trailog: %v", err)
	}
}

func run() error {
	// ── 1. Load configuration ────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log.Printf("trailog starting — %s", cfg)

	// ── 2. Open the raw *sql.DB (driver already imported above) ──
	db, err := openDB(cfg)
	if err != nil {
		return fmt.Errorf("db open: %w", err)
	}
	defer db.Close()

	// ── 3. Run migrations ────────────────────────────────────────
	if cfg.RunMigrations {
		dialect := toMigrateDialect(cfg.DBDriver)
		runner := migrate.New(db, dialect)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		n, err := runner.Run(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("migrations: %w", err)
		}
		if n > 0 {
			log.Printf("migrations: applied %d file(s)", n)
		} else {
			log.Println("migrations: schema up to date")
		}
	}

	// ── 4. Build Trailog library handle ───────────────────────────
	storeOpt := storeOptionForDriver(cfg)
	tlOpts := []trailog.TrailogOption{storeOpt}
	if cfg.Async {
		tlOpts = append(tlOpts, trailog.WithAsync(cfg.AsyncBuffer, cfg.AsyncWorkers, func(err error) {
			log.Printf("trailog async dispatch error: %v", err)
		}))
	}

	tl, err := trailog.New(tlOpts...)
	if err != nil {
		return fmt.Errorf("trailog.New: %w", err)
	}
	defer tl.Close()

	// ── 5. Start HTTP API server ──────────────────────────────────
	srvCfg := api.ServerConfig{
		Addr:         cfg.HTTPAddr,
		ReadTimeout:  cfg.HTTPReadTimeout,
		WriteTimeout: cfg.HTTPWriteTimeout,
		IdleTimeout:  cfg.HTTPIdleTimeout,
	}
	srv := api.NewServer(
		srvCfg,
		tl.Timeline(),
		tl.Reverter(),
		httpmw.ActorFromJWT, // swap for httpmw.ActorFromHeader for service-to-service
	)

	// ── 6. Graceful shutdown on SIGINT / SIGTERM ──────────────────
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Printf("received %s — shutting down gracefully …", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		log.Println("server stopped")
	}
	return nil
}

// ────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────

// openDB opens a *sql.DB for the configured driver and DSN.
// Drivers are registered via blank imports at the top of this file.
func openDB(cfg *config.Config) (*sql.DB, error) {
	driverName := string(cfg.DBDriver)
	// go-mssqldb registers under "sqlserver"; no rename needed.
	db, err := sql.Open(driverName, cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("sql.Open(%q): %w", driverName, err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping %s: %w", cfg.DBDriver, err)
	}
	log.Printf("connected to %s", cfg.DBDriver)
	return db, nil
}

// toMigrateDialect converts a config.DBDriver to a migrate.Dialect.
func toMigrateDialect(d config.DBDriver) migrate.Dialect {
	switch d {
	case config.DBDriverMySQL:
		return migrate.DialectMySQL
	case config.DBDriverSQLServer:
		return migrate.DialectSQLServer
	default:
		return migrate.DialectPostgres
	}
}

// storeOptionForDriver returns the trailog.TrailogOption that wires the correct
// store implementation for the configured driver, re-using the same DSN.
func storeOptionForDriver(cfg *config.Config) trailog.TrailogOption {
	switch cfg.DBDriver {
	case config.DBDriverMySQL:
		return trailog.WithMySQLStore(cfg.DBDSN)
	case config.DBDriverSQLServer:
		return trailog.WithSQLServerStore(cfg.DBDSN)
	default:
		return trailog.WithPostgresStore(cfg.DBDSN)
	}
}
