// Package config loads trailog runtime configuration from environment variables
// (with optional .env file support) and exposes a typed Config struct.
//
// Environment variables (all prefixed TRAILOG_):
//
//	TRAILOG_DB_DRIVER       postgres | mysql | sqlserver   (required)
//	TRAILOG_DB_DSN          database connection string      (required)
//	TRAILOG_RUN_MIGRATIONS  true | false  (default: true)
//	TRAILOG_HTTP_ADDR       listen address  (default: :8080)
//	TRAILOG_HTTP_READ_TIMEOUT   e.g. 15s   (default: 15s)
//	TRAILOG_HTTP_WRITE_TIMEOUT  e.g. 30s   (default: 30s)
//	TRAILOG_HTTP_IDLE_TIMEOUT   e.g. 60s   (default: 60s)
//	TRAILOG_ASYNC           true | false   (default: false — sync writes)
//	TRAILOG_ASYNC_BUFFER    int            (default: 512)
//	TRAILOG_ASYNC_WORKERS   int            (default: 4)
//	TRAILOG_LOG_LEVEL       debug | info | warn | error  (default: info)
//	TRAILOG_ENV_FILE        path to .env file  (default: .env — loaded only if file exists)
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DBDriver identifies the target database engine.
type DBDriver string

const (
	DBDriverPostgres  DBDriver = "postgres"
	DBDriverMySQL     DBDriver = "mysql"
	DBDriverSQLServer DBDriver = "sqlserver"
)

// Config holds all runtime configuration for the trailog service.
type Config struct {
	// Database
	DBDriver       DBDriver
	DBDSN          string
	RunMigrations  bool

	// HTTP server
	HTTPAddr         string
	HTTPReadTimeout  time.Duration
	HTTPWriteTimeout time.Duration
	HTTPIdleTimeout  time.Duration

	// Dispatcher
	Async        bool
	AsyncBuffer  int
	AsyncWorkers int

	// Observability
	LogLevel string
}

// Load reads configuration from environment variables, optionally loading a
// .env file first. The .env file path is read from TRAILOG_ENV_FILE (default ".env").
// Missing .env file is silently ignored; a parse error returns an error.
//
// This is the standard entry point when trailog is run as a standalone service
// (cmd/trailog). Embedding projects should use LoadFromFile or FromEnv instead.
func Load() (*Config, error) {
	// Load .env file if present.
	envFile := getEnv("TRAILOG_ENV_FILE", ".env")
	if err := loadDotEnv(envFile); err != nil {
		return nil, fmt.Errorf("config: load .env %q: %w", envFile, err)
	}
	return readEnvVars()
}

// LoadFromFile loads a specific .env file then reads TRAILOG_* env vars.
// Use this in an embedding project whose own main.go manages the .env path:
//
//	cfg, err := config.LoadFromFile("configs/audit.env")
//
// Pass an empty string to skip .env loading entirely and read only from the
// process environment (useful when the host app already loaded its own .env).
func LoadFromFile(envFilePath string) (*Config, error) {
	if envFilePath != "" {
		if err := loadDotEnv(envFilePath); err != nil {
			return nil, fmt.Errorf("config: load .env %q: %w", envFilePath, err)
		}
	}
	return readEnvVars()
}

// FromEnv reads TRAILOG_* variables directly from the process environment
// without touching any .env file. Use this when the host application already
// loaded its environment (e.g. via its own config library or Docker env-vars).
//
//	// host project main.go
//	cfg, err := config.FromEnv()
func FromEnv() (*Config, error) {
	return readEnvVars()
}

// readEnvVars reads all TRAILOG_* variables from the current process environment
// and returns a populated Config. Called by Load, LoadFromFile, and FromEnv.
func readEnvVars() (*Config, error) {
	c := &Config{}

	// ── Database ────────────────────────────────────────────────
	driver := strings.ToLower(strings.TrimSpace(getEnv("TRAILOG_DB_DRIVER", "")))
	switch DBDriver(driver) {
	case DBDriverPostgres, DBDriverMySQL, DBDriverSQLServer:
		c.DBDriver = DBDriver(driver)
	case "":
		return nil, fmt.Errorf("config: TRAILOG_DB_DRIVER is required (postgres | mysql | sqlserver)")
	default:
		return nil, fmt.Errorf("config: unsupported TRAILOG_DB_DRIVER %q — must be postgres, mysql, or sqlserver", driver)
	}

	c.DBDSN = getEnv("TRAILOG_DB_DSN", "")
	if c.DBDSN == "" {
		return nil, fmt.Errorf("config: TRAILOG_DB_DSN is required")
	}

	c.RunMigrations = getBool("TRAILOG_RUN_MIGRATIONS", true)

	// ── HTTP server ──────────────────────────────────────────────
	c.HTTPAddr = getEnv("TRAILOG_HTTP_ADDR", ":8080")
	c.HTTPReadTimeout = getDuration("TRAILOG_HTTP_READ_TIMEOUT", 15*time.Second)
	c.HTTPWriteTimeout = getDuration("TRAILOG_HTTP_WRITE_TIMEOUT", 30*time.Second)
	c.HTTPIdleTimeout = getDuration("TRAILOG_HTTP_IDLE_TIMEOUT", 60*time.Second)

	// ── Dispatcher ───────────────────────────────────────────────
	c.Async = getBool("TRAILOG_ASYNC", false)
	c.AsyncBuffer = getInt("TRAILOG_ASYNC_BUFFER", 512)
	c.AsyncWorkers = getInt("TRAILOG_ASYNC_WORKERS", 4)

	// ── Observability ────────────────────────────────────────────
	c.LogLevel = strings.ToLower(getEnv("TRAILOG_LOG_LEVEL", "info"))

	return c, nil
}

// Validate performs semantic validation beyond the type checks in Load.
func (c *Config) Validate() error {
	if c.AsyncBuffer < 1 {
		return fmt.Errorf("config: TRAILOG_ASYNC_BUFFER must be >= 1")
	}
	if c.AsyncWorkers < 1 {
		return fmt.Errorf("config: TRAILOG_ASYNC_WORKERS must be >= 1")
	}
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[c.LogLevel] {
		return fmt.Errorf("config: TRAILOG_LOG_LEVEL must be debug|info|warn|error, got %q", c.LogLevel)
	}
	return nil
}

// String returns a redacted summary (DSN is masked) for logging.
func (c *Config) String() string {
	return fmt.Sprintf(
		"driver=%s addr=%s async=%v migrations=%v log=%s",
		c.DBDriver, c.HTTPAddr, c.Async, c.RunMigrations, c.LogLevel,
	)
}

// ────────────────────────────────────────────────────────────────
// .env file loader
// ────────────────────────────────────────────────────────────────

// loadDotEnv parses a simple KEY=VALUE .env file and sets each variable in the
// process environment (only if it is not already set — existing env wins).
// Lines starting with # are comments. Blank lines are ignored.
// Values may optionally be wrapped in single or double quotes.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil // .env is optional
	}
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		idx := strings.IndexByte(line, '=')
		if idx < 1 {
			return fmt.Errorf("line %d: invalid format (expected KEY=VALUE)", lineNum)
		}

		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = unquote(val)

		// Don't override variables that are already set in the environment.
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, val); err != nil {
				return fmt.Errorf("line %d: setenv %q: %w", lineNum, key, err)
			}
		}
	}
	return scanner.Err()
}

// unquote strips surrounding single or double quotes from a value.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') ||
			(s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// ────────────────────────────────────────────────────────────────
// env helpers
// ────────────────────────────────────────────────────────────────

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func getBool(key string, fallback bool) bool {
	v := getEnv(key, "")
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func getInt(key string, fallback int) int {
	v := getEnv(key, "")
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func getDuration(key string, fallback time.Duration) time.Duration {
	v := getEnv(key, "")
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
