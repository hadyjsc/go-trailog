// Package migrate provides a dependency-free, pure database/sql migration runner
// for the trailog audit history library.
//
// How it works:
//  1. On first run it creates an audit_migrations tracking table (if absent).
//  2. It scans the embedded SQL files for the chosen dialect in lexicographic order.
//  3. Any file not yet recorded in audit_migrations is executed inside its own
//     transaction and then marked as applied. Already-applied files are skipped.
//
// Supported dialects: "postgres", "mysql", "sqlserver".
// Each dialect's SQL files live at:
//
//	store/postgres/migrations/*.sql
//	store/mysql/migrations/*.sql
//	store/sqlserver/migrations/*.sql
//
// The files are embedded at compile time via embed.FS so the binary is self-contained.
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

// ────────────────────────────────────────────────────────────────
// Embedded SQL files — one sub-FS per dialect
// ────────────────────────────────────────────────────────────────

//go:embed sql/postgres/*.sql
var postgresFS embed.FS

//go:embed sql/mysql/*.sql
var mysqlFS embed.FS

//go:embed sql/sqlserver/*.sql
var sqlserverFS embed.FS

// Dialect identifies the target database engine.
type Dialect string

const (
	DialectPostgres  Dialect = "postgres"
	DialectMySQL     Dialect = "mysql"
	DialectSQLServer Dialect = "sqlserver"
)

// Runner executes pending migrations against a live *sql.DB.
type Runner struct {
	db      *sql.DB
	dialect Dialect
}

// New creates a Runner for the given *sql.DB and dialect.
func New(db *sql.DB, dialect Dialect) *Runner {
	return &Runner{db: db, dialect: dialect}
}

// Run applies all pending migrations in order and returns the number of
// newly-applied files. It is safe to call on every startup (idempotent).
func (r *Runner) Run(ctx context.Context) (int, error) {
	if err := r.ensureTrackingTable(ctx); err != nil {
		return 0, fmt.Errorf("migrate: ensure tracking table: %w", err)
	}

	files, err := r.migrationFiles()
	if err != nil {
		return 0, fmt.Errorf("migrate: list files: %w", err)
	}

	applied := 0
	for _, f := range files {
		done, err := r.isApplied(ctx, f.name)
		if err != nil {
			return applied, fmt.Errorf("migrate: check %s: %w", f.name, err)
		}
		if done {
			continue
		}
		if err := r.applyFile(ctx, f); err != nil {
			return applied, fmt.Errorf("migrate: apply %s: %w", f.name, err)
		}
		applied++
	}
	return applied, nil
}

// MigrationStatus returns a list of all known migration files and whether each
// has been applied. Useful for health-check endpoints or CLI status commands.
func (r *Runner) MigrationStatus(ctx context.Context) ([]MigrationRecord, error) {
	if err := r.ensureTrackingTable(ctx); err != nil {
		return nil, err
	}

	files, err := r.migrationFiles()
	if err != nil {
		return nil, err
	}

	var records []MigrationRecord
	for _, f := range files {
		applied, err := r.isApplied(ctx, f.name)
		if err != nil {
			return nil, err
		}
		rec := MigrationRecord{Name: f.name, Applied: applied}
		if applied {
			rec.AppliedAt, _ = r.appliedAt(ctx, f.name)
		}
		records = append(records, rec)
	}
	return records, nil
}

// MigrationRecord describes the status of one migration file.
type MigrationRecord struct {
	Name      string
	Applied   bool
	AppliedAt time.Time
}

// ────────────────────────────────────────────────────────────────
// internal
// ────────────────────────────────────────────────────────────────

type migrationFile struct {
	name    string
	content []byte
}

func (r *Runner) migrationFiles() ([]migrationFile, error) {
	var (
		fsys   embed.FS
		prefix string
	)
	switch r.dialect {
	case DialectPostgres:
		fsys = postgresFS
		prefix = "sql/postgres"
	case DialectMySQL:
		fsys = mysqlFS
		prefix = "sql/mysql"
	case DialectSQLServer:
		fsys = sqlserverFS
		prefix = "sql/sqlserver"
	default:
		return nil, fmt.Errorf("migrate: unknown dialect %q", r.dialect)
	}

	entries, err := fs.ReadDir(fsys, prefix)
	if err != nil {
		return nil, fmt.Errorf("migrate: read dir %q: %w", prefix, err)
	}

	var files []migrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		content, err := fsys.ReadFile(prefix + "/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrate: read %q: %w", e.Name(), err)
		}
		files = append(files, migrationFile{name: e.Name(), content: content})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

func (r *Runner) ensureTrackingTable(ctx context.Context) error {
	var q string
	switch r.dialect {
	case DialectPostgres:
		q = `CREATE TABLE IF NOT EXISTS audit_migrations (
			name        TEXT        PRIMARY KEY,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`
	case DialectMySQL:
		q = `CREATE TABLE IF NOT EXISTS audit_migrations (
			name        VARCHAR(255) NOT NULL,
			applied_at  DATETIME(6)  NOT NULL DEFAULT (UTC_TIMESTAMP(6)),
			PRIMARY KEY (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	case DialectSQLServer:
		q = `IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'audit_migrations')
		CREATE TABLE audit_migrations (
			name        NVARCHAR(255)       NOT NULL,
			applied_at  DATETIMEOFFSET(6)   NOT NULL DEFAULT SYSDATETIMEOFFSET(),
			CONSTRAINT PK_audit_migrations PRIMARY KEY (name)
		)`
	default:
		return fmt.Errorf("migrate: unknown dialect %q", r.dialect)
	}
	_, err := r.db.ExecContext(ctx, q)
	return err
}

func (r *Runner) isApplied(ctx context.Context, name string) (bool, error) {
	var q string
	switch r.dialect {
	case DialectSQLServer:
		q = `SELECT COUNT(1) FROM audit_migrations WHERE name = @p1`
	default:
		q = `SELECT COUNT(1) FROM audit_migrations WHERE name = ?`
		if r.dialect == DialectPostgres {
			q = `SELECT COUNT(1) FROM audit_migrations WHERE name = $1`
		}
	}
	var count int
	if err := r.db.QueryRowContext(ctx, q, name).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *Runner) appliedAt(ctx context.Context, name string) (time.Time, error) {
	var q string
	switch r.dialect {
	case DialectSQLServer:
		q = `SELECT applied_at FROM audit_migrations WHERE name = @p1`
	case DialectPostgres:
		q = `SELECT applied_at FROM audit_migrations WHERE name = $1`
	default:
		q = `SELECT applied_at FROM audit_migrations WHERE name = ?`
	}
	var t time.Time
	if err := r.db.QueryRowContext(ctx, q, name).Scan(&t); err != nil {
		return time.Time{}, err
	}
	return t, nil
}

func (r *Runner) markApplied(ctx context.Context, tx *sql.Tx, name string) error {
	var q string
	switch r.dialect {
	case DialectSQLServer:
		q = `INSERT INTO audit_migrations (name) VALUES (@p1)`
	case DialectPostgres:
		q = `INSERT INTO audit_migrations (name) VALUES ($1)`
	default:
		q = `INSERT INTO audit_migrations (name) VALUES (?)`
	}
	_, err := tx.ExecContext(ctx, q, name)
	return err
}

// applyFile executes all statements in a migration file inside one transaction,
// then records the file in audit_migrations. SQL Server uses GO-separated batches
// which cannot run inside a single transaction — for SQL Server we execute each
// statement individually and skip the transaction wrapper.
func (r *Runner) applyFile(ctx context.Context, f migrationFile) error {
	statements := splitStatements(string(f.content), r.dialect)

	if r.dialect == DialectSQLServer {
		// SQL Server DDL (CREATE TABLE, CREATE INDEX) cannot always run in a
		// user transaction. Execute each batch independently.
		for _, stmt := range statements {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err := r.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("statement failed: %w\nSQL: %.200s", err, stmt)
			}
		}
		// Record completion outside a transaction.
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := r.markApplied(ctx, tx, f.name); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}

	// Postgres / MySQL: wrap everything in one transaction.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	for _, stmt := range statements {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("statement failed: %w\nSQL: %.200s", err, stmt)
		}
	}

	if err = r.markApplied(ctx, tx, f.name); err != nil {
		return err
	}
	return tx.Commit()
}

// splitStatements splits SQL source into individual executable statements.
// For SQL Server, the GO batch separator is used. For others, semicolons are used.
// Comments (-- line and /* block */) are preserved but don't affect splitting.
func splitStatements(src string, dialect Dialect) []string {
	if dialect == DialectSQLServer {
		// Split on GO on its own line (case-insensitive).
		var batches []string
		var current strings.Builder
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.EqualFold(trimmed, "go") {
				if b := strings.TrimSpace(current.String()); b != "" {
					batches = append(batches, b)
				}
				current.Reset()
			} else {
				current.WriteString(line)
				current.WriteByte('\n')
			}
		}
		if b := strings.TrimSpace(current.String()); b != "" {
			batches = append(batches, b)
		}
		return batches
	}

	// Postgres / MySQL: split on semicolons outside string literals.
	var stmts []string
	var current strings.Builder
	inLineComment := false
	inBlockComment := false
	runes := []rune(src)

	for i := 0; i < len(runes); i++ {
		ch := runes[i]

		// Handle line comment.
		if !inBlockComment && i+1 < len(runes) && ch == '-' && runes[i+1] == '-' {
			inLineComment = true
		}
		if inLineComment && ch == '\n' {
			inLineComment = false
		}

		// Handle block comment.
		if !inLineComment && i+1 < len(runes) && ch == '/' && runes[i+1] == '*' {
			inBlockComment = true
		}
		if inBlockComment && i > 0 && runes[i-1] == '*' && ch == '/' {
			inBlockComment = false
			current.WriteRune(ch)
			continue
		}

		if !inLineComment && !inBlockComment && ch == ';' {
			if stmt := strings.TrimSpace(current.String()); stmt != "" {
				stmts = append(stmts, stmt)
			}
			current.Reset()
			continue
		}
		current.WriteRune(ch)
	}
	if stmt := strings.TrimSpace(current.String()); stmt != "" {
		stmts = append(stmts, stmt)
	}
	return stmts
}
