// Package store persists users, channels, gateway keys and request logs in
// SQLite (the default, single-file deployment) or PostgreSQL (DATABASE_URL).
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/auth"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// ErrTokenInactive is returned when a gateway key was disabled before a
// reservation could be placed.
var ErrTokenInactive = errors.New("token is not active")

// ErrTokenExpired is returned when a gateway key expired before a reservation
// could be placed.
var ErrTokenExpired = errors.New("token has expired")

// InsufficientQuotaError reports a rejected quota reservation. It carries the
// figures used by the decision so the gateway can explain the rejection.
type InsufficientQuotaError struct {
	Required  float64
	Available float64
	Quota     float64
	Used      float64
	Reserved  float64
}

func (e *InsufficientQuotaError) Error() string {
	return "insufficient quota"
}

// Store wraps the configured database. Exactly one backend is active per
// process: SQLite for the default single-file deployment, or PostgreSQL when
// DATABASE_URL is set.
type Store struct {
	db      *conn
	dialect dialect
}

// Open opens (and migrates) the SQLite database at path. This is the original
// single-file deployment mode and stays the default when DATABASE_URL is unset.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// SQLite serializes writers; a single connection avoids lock churn while
	// keeping streaming requests (which do not hold a connection) fast.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	s := &Store{db: &conn{DB: db, dialect: dialectSQLite}, dialect: dialectSQLite}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// OpenPostgres opens (and migrates) a PostgreSQL database. databaseURL must be
// a postgres:// or postgresql:// URL, for example the DATABASE_URL of a
// dedicated exp_* database. The URL is never logged by this package.
func OpenPostgres(databaseURL string) (*Store, error) {
	if _, err := pgx.ParseConfig(databaseURL); err != nil {
		// Do not wrap the driver error: it can echo the connection string,
		// including the database password, into logs.
		return nil, fmt.Errorf("invalid DATABASE_URL")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open postgres database: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres database: %w", err)
	}
	s := &Store{db: &conn{DB: db, dialect: dialectPostgres}, dialect: dialectPostgres}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Backend reports the active database backend ("sqlite" or "postgres").
func (s *Store) Backend() string { return s.dialect.String() }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if s.dialect == dialectPostgres {
		return s.migratePostgres()
	}
	return s.migrateSQLite()
}

func (s *Store) migrateSQLite() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'admin',
			status TEXT NOT NULL DEFAULT 'enabled',
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS channels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			base_url TEXT NOT NULL,
			api_key TEXT NOT NULL,
			api_key_hint TEXT NOT NULL DEFAULT '',
			models TEXT NOT NULL DEFAULT '[]',
			model_mapping TEXT NOT NULL DEFAULT '{}',
			priority INTEGER NOT NULL DEFAULT 0,
			weight INTEGER NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'enabled',
			input_price REAL NOT NULL DEFAULT 0,
			output_price REAL NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			key_hash TEXT NOT NULL UNIQUE,
			key_prefix TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			quota REAL NOT NULL DEFAULT -1,
			used_quota REAL NOT NULL DEFAULT 0,
			allowed_models TEXT NOT NULL DEFAULT '[]',
			rpm_limit INTEGER NOT NULL DEFAULT 0,
			tpm_limit INTEGER NOT NULL DEFAULT 0,
			max_concurrency INTEGER NOT NULL DEFAULT 0,
			daily_quota REAL NOT NULL DEFAULT 0,
			expires_at INTEGER,
			last_used_at INTEGER,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT NOT NULL,
			user_id INTEGER NOT NULL DEFAULT 0,
			token_id INTEGER NOT NULL DEFAULT 0,
			channel_id INTEGER NOT NULL DEFAULT 0,
			token_name TEXT NOT NULL DEFAULT '',
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0,
			reserved_cost REAL NOT NULL DEFAULT 0,
			cost_status TEXT NOT NULL DEFAULT 'actual',
			reservation_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'success',
			status_code INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			latency_ms INTEGER NOT NULL DEFAULT 0,
			stream INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_created_at ON logs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_token_id ON logs(token_id)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_model ON logs(model)`,
		`CREATE TABLE IF NOT EXISTS token_reservations (
			id TEXT PRIMARY KEY,
			token_id INTEGER NOT NULL,
			token_name TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			reserved_cost REAL NOT NULL DEFAULT 0,
			settled_cost REAL NOT NULL DEFAULT 0,
			cost_status TEXT NOT NULL DEFAULT 'pending',
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			channel_id INTEGER NOT NULL DEFAULT 0,
			channel_name TEXT NOT NULL DEFAULT '',
			stream INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			settled_at INTEGER,
			expires_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_reservations_token_status ON token_reservations(token_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_reservations_status ON token_reservations(status)`,
		`CREATE INDEX IF NOT EXISTS idx_reservations_created_at ON token_reservations(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_token_created_at ON logs(token_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS video_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			upstream_task_id TEXT NOT NULL DEFAULT '',
			channel_id INTEGER NOT NULL DEFAULT 0,
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL DEFAULT 'text',
			resolution TEXT NOT NULL DEFAULT '',
			duration INTEGER NOT NULL DEFAULT 0,
			ratio TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'queued',
			error TEXT NOT NULL DEFAULT '',
			video_path TEXT NOT NULL DEFAULT '',
			remote_url TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_video_tasks_created_at ON video_tasks(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_video_tasks_status ON video_tasks(status)`,
		`CREATE TABLE IF NOT EXISTS speech_clips (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			channel_id INTEGER NOT NULL DEFAULT 0,
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			voice_id TEXT NOT NULL DEFAULT '',
			voice_name TEXT NOT NULL DEFAULT '',
			text TEXT NOT NULL DEFAULT '',
			speed REAL NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'succeeded',
			error TEXT NOT NULL DEFAULT '',
			total_chunks INTEGER NOT NULL DEFAULT 0,
			done_chunks INTEGER NOT NULL DEFAULT 0,
			upstream_task_id TEXT NOT NULL DEFAULT '',
			upstream_file_id INTEGER NOT NULL DEFAULT 0,
			format TEXT NOT NULL DEFAULT 'mp3',
			audio_path TEXT NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			characters INTEGER NOT NULL DEFAULT 0,
			size_bytes INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_speech_clips_created_at ON speech_clips(created_at)`,
		`CREATE TABLE IF NOT EXISTS voice_clones (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			voice_id TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			channel_id INTEGER NOT NULL DEFAULT 0,
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			sample_format TEXT NOT NULL DEFAULT '',
			sample_bytes INTEGER NOT NULL DEFAULT 0,
			source_filename TEXT NOT NULL DEFAULT '',
			preview_text TEXT NOT NULL DEFAULT '',
			preview_path TEXT NOT NULL DEFAULT '',
			preview_format TEXT NOT NULL DEFAULT '',
			preview_duration_ms INTEGER NOT NULL DEFAULT 0,
			preview_characters INTEGER NOT NULL DEFAULT 0,
			authorized INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_voice_clones_created_at ON voice_clones(created_at)`,
		`CREATE TABLE IF NOT EXISTS voice_designs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			channel_id INTEGER NOT NULL DEFAULT 0,
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			voice_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			preview_text TEXT NOT NULL DEFAULT '',
			preview_path TEXT NOT NULL DEFAULT '',
			preview_format TEXT NOT NULL DEFAULT 'mp3',
			preview_duration_ms INTEGER NOT NULL DEFAULT 0,
			preview_characters INTEGER NOT NULL DEFAULT 0,
			preview_size_bytes INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_voice_designs_created_at ON voice_designs(created_at)`,
		`CREATE TABLE IF NOT EXISTS music_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			channel_id INTEGER NOT NULL DEFAULT 0,
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			lyrics TEXT NOT NULL DEFAULT '',
			instrumental INTEGER NOT NULL DEFAULT 0,
			format TEXT NOT NULL DEFAULT 'mp3',
			status TEXT NOT NULL DEFAULT 'queued',
			error TEXT NOT NULL DEFAULT '',
			audio_path TEXT NOT NULL DEFAULT '',
			remote_url TEXT NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_music_tasks_created_at ON music_tasks(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_music_tasks_status ON music_tasks(status)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_request_id ON logs(request_id)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
	}
	// Columns added after the first release. SQLite has no
	// "ADD COLUMN IF NOT EXISTS", so inspect the schema first.
	for _, column := range []struct{ name, definition string }{
		{"cost_status", `TEXT NOT NULL DEFAULT 'actual'`},
		{"reserved_cost", `REAL NOT NULL DEFAULT 0`},
		{"reservation_id", `TEXT NOT NULL DEFAULT ''`},
	} {
		if err := s.ensureColumn("logs", column.name, column.definition); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
	}
	for _, column := range []struct{ name, definition string }{
		{"tpm_limit", `INTEGER NOT NULL DEFAULT 0`},
		{"max_concurrency", `INTEGER NOT NULL DEFAULT 0`},
		{"daily_quota", `REAL NOT NULL DEFAULT 0`},
	} {
		if err := s.ensureColumn("tokens", column.name, column.definition); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
	}
	// speech_clips predates background jobs: older rows hold finished clips and
	// keep the 'succeeded' default once these columns are added.
	for _, column := range []struct{ name, definition string }{
		{"speed", `REAL NOT NULL DEFAULT 1`},
		{"status", `TEXT NOT NULL DEFAULT 'succeeded'`},
		{"error", `TEXT NOT NULL DEFAULT ''`},
		{"total_chunks", `INTEGER NOT NULL DEFAULT 0`},
		{"done_chunks", `INTEGER NOT NULL DEFAULT 0`},
		{"upstream_task_id", `TEXT NOT NULL DEFAULT ''`},
		{"upstream_file_id", `INTEGER NOT NULL DEFAULT 0`},
	} {
		if err := s.ensureColumn("speech_clips", column.name, column.definition); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_speech_clips_status ON speech_clips(status)`); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	return nil
}

// migratePostgres creates the same schema as migrateSQLite using PostgreSQL
// types: BIGSERIAL ids, BIGINT timestamps and counters, DOUBLE PRECISION money
// and costs. Existing SQLite installs are untouched; an empty PostgreSQL
// database is initialized on first start and then migrated in place.
func (s *Store) migratePostgres() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id BIGSERIAL PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'admin',
			status TEXT NOT NULL DEFAULT 'enabled',
			created_at BIGINT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS channels (
			id BIGSERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			base_url TEXT NOT NULL,
			api_key TEXT NOT NULL,
			api_key_hint TEXT NOT NULL DEFAULT '',
			models TEXT NOT NULL DEFAULT '[]',
			model_mapping TEXT NOT NULL DEFAULT '{}',
			priority BIGINT NOT NULL DEFAULT 0,
			weight BIGINT NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'enabled',
			input_price DOUBLE PRECISION NOT NULL DEFAULT 0,
			output_price DOUBLE PRECISION NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tokens (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL,
			name TEXT NOT NULL,
			key_hash TEXT NOT NULL UNIQUE,
			key_prefix TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			quota DOUBLE PRECISION NOT NULL DEFAULT -1,
			used_quota DOUBLE PRECISION NOT NULL DEFAULT 0,
			allowed_models TEXT NOT NULL DEFAULT '[]',
			rpm_limit BIGINT NOT NULL DEFAULT 0,
			tpm_limit BIGINT NOT NULL DEFAULT 0,
			max_concurrency BIGINT NOT NULL DEFAULT 0,
			daily_quota DOUBLE PRECISION NOT NULL DEFAULT 0,
			expires_at BIGINT,
			last_used_at BIGINT,
			created_at BIGINT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS logs (
			id BIGSERIAL PRIMARY KEY,
			request_id TEXT NOT NULL,
			user_id BIGINT NOT NULL DEFAULT 0,
			token_id BIGINT NOT NULL DEFAULT 0,
			channel_id BIGINT NOT NULL DEFAULT 0,
			token_name TEXT NOT NULL DEFAULT '',
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			prompt_tokens BIGINT NOT NULL DEFAULT 0,
			completion_tokens BIGINT NOT NULL DEFAULT 0,
			total_tokens BIGINT NOT NULL DEFAULT 0,
			cost DOUBLE PRECISION NOT NULL DEFAULT 0,
			reserved_cost DOUBLE PRECISION NOT NULL DEFAULT 0,
			cost_status TEXT NOT NULL DEFAULT 'actual',
			reservation_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'success',
			status_code BIGINT NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			latency_ms BIGINT NOT NULL DEFAULT 0,
			stream BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_created_at ON logs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_token_id ON logs(token_id)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_model ON logs(model)`,
		`CREATE TABLE IF NOT EXISTS token_reservations (
			id TEXT PRIMARY KEY,
			token_id BIGINT NOT NULL,
			token_name TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			reserved_cost DOUBLE PRECISION NOT NULL DEFAULT 0,
			settled_cost DOUBLE PRECISION NOT NULL DEFAULT 0,
			cost_status TEXT NOT NULL DEFAULT 'pending',
			prompt_tokens BIGINT NOT NULL DEFAULT 0,
			completion_tokens BIGINT NOT NULL DEFAULT 0,
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			channel_id BIGINT NOT NULL DEFAULT 0,
			channel_name TEXT NOT NULL DEFAULT '',
			stream BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			settled_at BIGINT,
			expires_at BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_reservations_token_status ON token_reservations(token_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_reservations_status ON token_reservations(status)`,
		`CREATE INDEX IF NOT EXISTS idx_reservations_created_at ON token_reservations(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_token_created_at ON logs(token_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS video_tasks (
			id BIGSERIAL PRIMARY KEY,
			upstream_task_id TEXT NOT NULL DEFAULT '',
			channel_id BIGINT NOT NULL DEFAULT 0,
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL DEFAULT 'text',
			resolution TEXT NOT NULL DEFAULT '',
			duration BIGINT NOT NULL DEFAULT 0,
			ratio TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'queued',
			error TEXT NOT NULL DEFAULT '',
			video_path TEXT NOT NULL DEFAULT '',
			remote_url TEXT NOT NULL DEFAULT '',
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_video_tasks_created_at ON video_tasks(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_video_tasks_status ON video_tasks(status)`,
		`CREATE TABLE IF NOT EXISTS speech_clips (
			id BIGSERIAL PRIMARY KEY,
			channel_id BIGINT NOT NULL DEFAULT 0,
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			voice_id TEXT NOT NULL DEFAULT '',
			voice_name TEXT NOT NULL DEFAULT '',
			text TEXT NOT NULL DEFAULT '',
			speed DOUBLE PRECISION NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'succeeded',
			error TEXT NOT NULL DEFAULT '',
			total_chunks BIGINT NOT NULL DEFAULT 0,
			done_chunks BIGINT NOT NULL DEFAULT 0,
			upstream_task_id TEXT NOT NULL DEFAULT '',
			upstream_file_id BIGINT NOT NULL DEFAULT 0,
			format TEXT NOT NULL DEFAULT 'mp3',
			audio_path TEXT NOT NULL DEFAULT '',
			duration_ms BIGINT NOT NULL DEFAULT 0,
			characters BIGINT NOT NULL DEFAULT 0,
			size_bytes BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_speech_clips_created_at ON speech_clips(created_at)`,
		`CREATE TABLE IF NOT EXISTS music_tasks (
			id BIGSERIAL PRIMARY KEY,
			channel_id BIGINT NOT NULL DEFAULT 0,
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			lyrics TEXT NOT NULL DEFAULT '',
			instrumental BIGINT NOT NULL DEFAULT 0,
			format TEXT NOT NULL DEFAULT 'mp3',
			status TEXT NOT NULL DEFAULT 'queued',
			error TEXT NOT NULL DEFAULT '',
			audio_path TEXT NOT NULL DEFAULT '',
			remote_url TEXT NOT NULL DEFAULT '',
			duration_ms BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_music_tasks_created_at ON music_tasks(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_music_tasks_status ON music_tasks(status)`,
		`CREATE TABLE IF NOT EXISTS voice_clones (
			id BIGSERIAL PRIMARY KEY,
			voice_id TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			channel_id BIGINT NOT NULL DEFAULT 0,
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			sample_format TEXT NOT NULL DEFAULT '',
			sample_bytes BIGINT NOT NULL DEFAULT 0,
			source_filename TEXT NOT NULL DEFAULT '',
			preview_text TEXT NOT NULL DEFAULT '',
			preview_path TEXT NOT NULL DEFAULT '',
			preview_format TEXT NOT NULL DEFAULT '',
			preview_duration_ms BIGINT NOT NULL DEFAULT 0,
			preview_characters BIGINT NOT NULL DEFAULT 0,
			authorized BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_voice_clones_created_at ON voice_clones(created_at)`,
		`CREATE TABLE IF NOT EXISTS voice_designs (
			id BIGSERIAL PRIMARY KEY,
			channel_id BIGINT NOT NULL DEFAULT 0,
			channel_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			upstream_model TEXT NOT NULL DEFAULT '',
			voice_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			preview_text TEXT NOT NULL DEFAULT '',
			preview_path TEXT NOT NULL DEFAULT '',
			preview_format TEXT NOT NULL DEFAULT 'mp3',
			preview_duration_ms BIGINT NOT NULL DEFAULT 0,
			preview_characters BIGINT NOT NULL DEFAULT 0,
			preview_size_bytes BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_voice_designs_created_at ON voice_designs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_request_id ON logs(request_id)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate postgres database: %w", err)
		}
	}
	// PostgreSQL supports ADD COLUMN IF NOT EXISTS, so legacy columns can be
	// filled in without inspecting information_schema on every start.
	for _, column := range []struct{ table, name, definition string }{
		{"logs", "cost_status", `TEXT NOT NULL DEFAULT 'actual'`},
		{"logs", "reserved_cost", `DOUBLE PRECISION NOT NULL DEFAULT 0`},
		{"logs", "reservation_id", `TEXT NOT NULL DEFAULT ''`},
		{"tokens", "tpm_limit", `BIGINT NOT NULL DEFAULT 0`},
		{"tokens", "max_concurrency", `BIGINT NOT NULL DEFAULT 0`},
		{"tokens", "daily_quota", `DOUBLE PRECISION NOT NULL DEFAULT 0`},
		{"speech_clips", "speed", `DOUBLE PRECISION NOT NULL DEFAULT 1`},
		{"speech_clips", "status", `TEXT NOT NULL DEFAULT 'succeeded'`},
		{"speech_clips", "error", `TEXT NOT NULL DEFAULT ''`},
		{"speech_clips", "total_chunks", `BIGINT NOT NULL DEFAULT 0`},
		{"speech_clips", "done_chunks", `BIGINT NOT NULL DEFAULT 0`},
		{"speech_clips", "upstream_task_id", `TEXT NOT NULL DEFAULT ''`},
		{"speech_clips", "upstream_file_id", `BIGINT NOT NULL DEFAULT 0`},
	} {
		if err := s.ensureColumn(column.table, column.name, column.definition); err != nil {
			return fmt.Errorf("migrate postgres database: %w", err)
		}
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_speech_clips_status ON speech_clips(status)`); err != nil {
		return fmt.Errorf("migrate postgres database: %w", err)
	}
	return nil
}

// ensureColumn adds a column to an existing table when it is missing.
func (s *Store) ensureColumn(table, column, definition string) error {
	if s.dialect == dialectPostgres {
		definition = strings.ReplaceAll(definition, "REAL", "DOUBLE PRECISION")
		_, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN IF NOT EXISTS ` + column + ` ` + definition)
		return err
	}
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}

// User is an admin account.
type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	Role         string `json:"role"`
	Status       string `json:"status"`
	CreatedAt    int64  `json:"created_at"`
}

// EnsureAdmin creates the initial admin account when no user exists.
func (s *Store) EnsureAdmin(username, password string) (bool, error) {
	return s.ensureAdmin(username, password, false)
}

// EnsureTestAdmin creates the initial admin account for the explicitly
// enabled local test environment. It is the only path that accepts a password
// shorter than the production minimum (TEST_MODE uses admin/admin).
func (s *Store) EnsureTestAdmin(username, password string) (bool, error) {
	return s.ensureAdmin(username, password, true)
}

func (s *Store) ensureAdmin(username, password string, testMode bool) (bool, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return false, err
	}
	if count > 0 {
		return false, nil
	}
	hashFn := auth.HashPassword
	if testMode {
		hashFn = auth.HashTestPassword
	}
	hash, err := hashFn(password)
	if err != nil {
		return false, err
	}
	_, err = s.db.Exec(`INSERT INTO users (username, password_hash, role, status, created_at) VALUES (?, ?, 'admin', 'enabled', ?)`,
		username, hash, now())
	return err == nil, err
}

// GetUserByUsername looks up a user by username.
func (s *Store) GetUserByUsername(username string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT id, username, password_hash, role, status, created_at FROM users WHERE username = ?`, username))
}

// GetUserByID looks up a user by id.
func (s *Store) GetUserByID(id int64) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT id, username, password_hash, role, status, created_at FROM users WHERE id = ?`, id))
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Status, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// Channel is an upstream provider configuration.
type Channel struct {
	ID              int64             `json:"id"`
	Name            string            `json:"name"`
	Type            string            `json:"type"`
	BaseURL         string            `json:"base_url"`
	APIKeyEncrypted string            `json:"-"`
	APIKeyHint      string            `json:"api_key_hint"`
	Models          []string          `json:"models"`
	ModelMapping    map[string]string `json:"model_mapping"`
	Priority        int               `json:"priority"`
	Weight          int               `json:"weight"`
	Status          string            `json:"status"`
	InputPrice      float64           `json:"input_price"`
	OutputPrice     float64           `json:"output_price"`
	CreatedAt       int64             `json:"created_at"`
	UpdatedAt       int64             `json:"updated_at"`
}

const channelColumns = `id, name, type, base_url, api_key, api_key_hint, models, model_mapping, priority, weight, status, input_price, output_price, created_at, updated_at`

// ListChannels returns all channels ordered by priority and id.
func (s *Store) ListChannels() ([]Channel, error) {
	rows, err := s.db.Query(`SELECT ` + channelColumns + ` FROM channels ORDER BY priority DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Channel, 0)
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// GetChannel returns a channel by id.
func (s *Store) GetChannel(id int64) (*Channel, error) {
	row := s.db.QueryRow(`SELECT `+channelColumns+` FROM channels WHERE id = ?`, id)
	c, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

func scanChannel(row interface{ Scan(dest ...any) error }) (*Channel, error) {
	var c Channel
	var models, mapping string
	err := row.Scan(&c.ID, &c.Name, &c.Type, &c.BaseURL, &c.APIKeyEncrypted, &c.APIKeyHint,
		&models, &mapping, &c.Priority, &c.Weight, &c.Status, &c.InputPrice, &c.OutputPrice, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.Models = decodeStrings(models)
	c.ModelMapping = decodeMap(mapping)
	return &c, nil
}

// CreateChannel inserts a channel and sets its id.
func (s *Store) CreateChannel(c *Channel) error {
	now := now()
	c.CreatedAt, c.UpdatedAt = now, now
	var err error
	c.ID, err = s.insertStoreID(`INSERT INTO channels (name, type, base_url, api_key, api_key_hint, models, model_mapping, priority, weight, status, input_price, output_price, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Type, c.BaseURL, c.APIKeyEncrypted, c.APIKeyHint, encodeStrings(c.Models), encodeMap(c.ModelMapping),
		c.Priority, c.Weight, c.Status, c.InputPrice, c.OutputPrice, c.CreatedAt, c.UpdatedAt)
	return err
}

// UpdateChannel persists all mutable channel fields.
func (s *Store) UpdateChannel(c *Channel) error {
	c.UpdatedAt = now()
	_, err := s.db.Exec(`UPDATE channels SET name=?, type=?, base_url=?, api_key=?, api_key_hint=?, models=?, model_mapping=?, priority=?, weight=?, status=?, input_price=?, output_price=?, updated_at=? WHERE id=?`,
		c.Name, c.Type, c.BaseURL, c.APIKeyEncrypted, c.APIKeyHint, encodeStrings(c.Models), encodeMap(c.ModelMapping),
		c.Priority, c.Weight, c.Status, c.InputPrice, c.OutputPrice, c.UpdatedAt, c.ID)
	return err
}

// DeleteChannel removes a channel.
func (s *Store) DeleteChannel(id int64) error {
	res, err := s.db.Exec(`DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Token is a gateway API key bound to a user.
type Token struct {
	ID             int64    `json:"id"`
	UserID         int64    `json:"user_id"`
	Name           string   `json:"name"`
	KeyHash        string   `json:"-"`
	KeyPrefix      string   `json:"key_prefix"`
	Status         string   `json:"status"`
	Quota          float64  `json:"quota"`
	UsedQuota      float64  `json:"used_quota"`
	ReservedQuota  float64  `json:"reserved_quota"`
	DailyQuota     float64  `json:"daily_quota"`
	DailyUsedQuota float64  `json:"daily_used_quota"`
	AllowedModels  []string `json:"allowed_models"`
	RPMLimit       int      `json:"rpm_limit"`
	TPMLimit       int      `json:"tpm_limit"`
	MaxConcurrency int      `json:"max_concurrency"`
	ExpiresAt      *int64   `json:"expires_at"`
	LastUsedAt     *int64   `json:"last_used_at"`
	CreatedAt      int64    `json:"created_at"`
}

const tokenColumns = `id, user_id, name, key_hash, key_prefix, status, quota, used_quota,
	COALESCE((SELECT SUM(reserved_cost) FROM token_reservations WHERE token_id = tokens.id AND status = 'pending'), 0),
	daily_quota, allowed_models, rpm_limit, tpm_limit, max_concurrency, expires_at, last_used_at, created_at`

// ListTokens returns every gateway key with today's spend attached.
func (s *Store) ListTokens() ([]Token, error) {
	rows, err := s.db.Query(`SELECT ` + tokenColumns + ` FROM tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Token, 0)
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	daily, err := s.tokenDailySpend(startOfDay(now()))
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].DailyUsedQuota = daily[out[i].ID]
	}
	return out, nil
}

// tokenDailySpend returns today's spend per key since the given unix timestamp.
func (s *Store) tokenDailySpend(since int64) (map[int64]float64, error) {
	rows, err := s.db.Query(`SELECT token_id, COALESCE(SUM(cost), 0) FROM logs
		WHERE created_at >= ? GROUP BY token_id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var id int64
		var total float64
		if err := rows.Scan(&id, &total); err != nil {
			return nil, err
		}
		out[id] = total
	}
	return out, rows.Err()
}

// TokenSpendSince sums the cost of successful requests for a key since a unix
// timestamp. It backs the per-key daily spending limit.
func (s *Store) TokenSpendSince(id int64, since int64) (float64, error) {
	var total float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost), 0) FROM logs
		WHERE token_id = ? AND created_at >= ?`, id, since).Scan(&total)
	return total, err
}

// startOfDay returns the unix timestamp of local midnight for t.
func startOfDay(t int64) int64 {
	tm := time.Unix(t, 0)
	return time.Date(tm.Year(), tm.Month(), tm.Day(), 0, 0, 0, 0, tm.Location()).Unix()
}

// GetTokenByID returns a key by id.
func (s *Store) GetTokenByID(id int64) (*Token, error) {
	t, err := scanToken(s.db.QueryRow(`SELECT `+tokenColumns+` FROM tokens WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// GetTokenByHash returns a key by its sha256 hash.
func (s *Store) GetTokenByHash(hash string) (*Token, error) {
	t, err := scanToken(s.db.QueryRow(`SELECT `+tokenColumns+` FROM tokens WHERE key_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func scanToken(row interface{ Scan(dest ...any) error }) (*Token, error) {
	var t Token
	var allowed string
	var expires, lastUsed sql.NullInt64
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.KeyHash, &t.KeyPrefix, &t.Status, &t.Quota, &t.UsedQuota, &t.ReservedQuota, &t.DailyQuota,
		&allowed, &t.RPMLimit, &t.TPMLimit, &t.MaxConcurrency, &expires, &lastUsed, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	t.AllowedModels = decodeStrings(allowed)
	if expires.Valid {
		v := expires.Int64
		t.ExpiresAt = &v
	}
	if lastUsed.Valid {
		v := lastUsed.Int64
		t.LastUsedAt = &v
	}
	return &t, nil
}

// CreateToken inserts a key.
func (s *Store) CreateToken(t *Token) error {
	t.CreatedAt = now()
	var err error
	t.ID, err = s.insertStoreID(`INSERT INTO tokens (user_id, name, key_hash, key_prefix, status, quota, used_quota, daily_quota, allowed_models, rpm_limit, tpm_limit, max_concurrency, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.UserID, t.Name, t.KeyHash, t.KeyPrefix, t.Status, t.Quota, t.UsedQuota, t.DailyQuota, encodeStrings(t.AllowedModels), t.RPMLimit, t.TPMLimit, t.MaxConcurrency, t.ExpiresAt, t.CreatedAt)
	return err
}

// UpdateToken persists mutable key fields.
func (s *Store) UpdateToken(t *Token) error {
	_, err := s.db.Exec(`UPDATE tokens SET name=?, status=?, quota=?, daily_quota=?, allowed_models=?, rpm_limit=?, tpm_limit=?, max_concurrency=?, expires_at=? WHERE id=?`,
		t.Name, t.Status, t.Quota, t.DailyQuota, encodeStrings(t.AllowedModels), t.RPMLimit, t.TPMLimit, t.MaxConcurrency, t.ExpiresAt, t.ID)
	return err
}

// DeleteToken removes a key.
func (s *Store) DeleteToken(id int64) error {
	res, err := s.db.Exec(`DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkTokenUsed records that a key authenticated a request.
func (s *Store) MarkTokenUsed(id int64) error {
	_, err := s.db.Exec(`UPDATE tokens SET last_used_at = ? WHERE id = ?`, now(), id)
	return err
}

// AddTokenUsage increments the key's consumed quota.
func (s *Store) AddTokenUsage(id int64, cost float64) error {
	_, err := s.db.Exec(`UPDATE tokens SET used_quota = used_quota + ?, last_used_at = ? WHERE id = ?`, cost, now(), id)
	return err
}

// Log is one relayed request.
type Log struct {
	ID               int64   `json:"id"`
	RequestID        string  `json:"request_id"`
	ReservationID    string  `json:"reservation_id"`
	UserID           int64   `json:"user_id"`
	TokenID          int64   `json:"token_id"`
	ChannelID        int64   `json:"channel_id"`
	TokenName        string  `json:"token_name"`
	ChannelName      string  `json:"channel_name"`
	Model            string  `json:"model"`
	UpstreamModel    string  `json:"upstream_model"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Cost             float64 `json:"cost"`
	ReservedCost     float64 `json:"reserved_cost"`
	CostStatus       string  `json:"cost_status"`
	Status           string  `json:"status"`
	StatusCode       int     `json:"status_code"`
	Error            string  `json:"error"`
	LatencyMS        int64   `json:"latency_ms"`
	Stream           bool    `json:"stream"`
	CreatedAt        int64   `json:"created_at"`
}

// InsertLog persists a request log entry.
func (s *Store) InsertLog(l *Log) error {
	if l.CreatedAt == 0 {
		l.CreatedAt = now()
	}
	if l.CostStatus == "" {
		l.CostStatus = "actual"
	}
	var err error
	l.ID, err = s.insertStoreID(`INSERT INTO logs (request_id, reservation_id, user_id, token_id, channel_id, token_name, channel_name, model, upstream_model,
		prompt_tokens, completion_tokens, total_tokens, cost, reserved_cost, cost_status, status, status_code, error, latency_ms, stream, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.RequestID, l.ReservationID, l.UserID, l.TokenID, l.ChannelID, l.TokenName, l.ChannelName, l.Model, l.UpstreamModel,
		l.PromptTokens, l.CompletionTokens, l.TotalTokens, l.Cost, l.ReservedCost, l.CostStatus, l.Status, l.StatusCode, l.Error, l.LatencyMS, boolValue(l.Stream), l.CreatedAt)
	return err
}

// LogFilter selects logs for the admin API.
type LogFilter struct {
	Page      int
	PageSize  int
	Model     string
	Status    string
	RequestID string
	TokenID   int64
	Start     int64
	End       int64
}

// ModelCost aggregates the logs of one model that match a filter.
type ModelCost struct {
	Model          string  `json:"model"`
	Requests       int64   `json:"requests"`
	Cost           float64 `json:"cost"`
	UnverifiedCost float64 `json:"unverified_cost"`
}

// LogSummary aggregates every log that matches a filter, not just one page.
type LogSummary struct {
	Requests       int64       `json:"requests"`
	Cost           float64     `json:"cost"`
	UnverifiedCost float64     `json:"unverified_cost"`
	ByModel        []ModelCost `json:"by_model"`
}

// LogPage is one page of logs plus aggregate totals across every match.
type LogPage struct {
	Items   []Log
	Summary LogSummary
}

// logColumns is the column list scanLog expects.
const logColumns = `id, request_id, reservation_id, user_id, token_id, channel_id, token_name, channel_name, model, upstream_model,
	prompt_tokens, completion_tokens, total_tokens, cost, reserved_cost, cost_status, status, status_code, error, latency_ms, stream, created_at`

// logWhere builds the WHERE clause shared by log listing, aggregation and exports.
func logWhere(f LogFilter) (string, []any) {
	var clauses []string
	var args []any
	if f.Model != "" {
		clauses = append(clauses, `model = ?`)
		args = append(args, f.Model)
	}
	if f.Status != "" {
		clauses = append(clauses, `status = ?`)
		args = append(args, f.Status)
	}
	if f.RequestID != "" {
		clauses = append(clauses, `request_id = ?`)
		args = append(args, f.RequestID)
	}
	if f.TokenID > 0 {
		clauses = append(clauses, `token_id = ?`)
		args = append(args, f.TokenID)
	}
	if f.Start > 0 {
		clauses = append(clauses, `created_at >= ?`)
		args = append(args, f.Start)
	}
	if f.End > 0 {
		clauses = append(clauses, `created_at <= ?`)
		args = append(args, f.End)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + joinAnd(clauses), args
}

// ListLogs returns one page of logs and the total number of matches.
func (s *Store) ListLogs(f LogFilter) ([]Log, int64, error) {
	page, err := s.ListLogsPage(f)
	if err != nil {
		return nil, 0, err
	}
	return page.Items, page.Summary.Requests, nil
}

// ListLogsPage returns one page of logs plus the totals across every log
// matching the filter, so totals stay stable while paging.
func (s *Store) ListLogsPage(f LogFilter) (*LogPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 200 {
		f.PageSize = 50
	}
	where, args := logWhere(f)
	summary := LogSummary{ByModel: make([]ModelCost, 0)}
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(cost), 0),
		COALESCE(SUM(CASE WHEN cost_status = 'unverified' THEN cost ELSE 0 END), 0)
		FROM logs`+where, args...).
		Scan(&summary.Requests, &summary.Cost, &summary.UnverifiedCost); err != nil {
		return nil, err
	}
	modelRows, err := s.db.Query(`SELECT model, COUNT(*), COALESCE(SUM(cost), 0),
		COALESCE(SUM(CASE WHEN cost_status = 'unverified' THEN cost ELSE 0 END), 0)
		FROM logs`+where+` GROUP BY model ORDER BY SUM(cost) DESC, COUNT(*) DESC, model ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var item ModelCost
		if err := modelRows.Scan(&item.Model, &item.Requests, &item.Cost, &item.UnverifiedCost); err != nil {
			return nil, err
		}
		summary.ByModel = append(summary.ByModel, item)
	}
	if err := modelRows.Err(); err != nil {
		return nil, err
	}
	queryArgs := append(append([]any{}, args...), f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := s.db.Query(`SELECT `+logColumns+` FROM logs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Log, 0)
	for rows.Next() {
		l, err := scanLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &LogPage{Items: out, Summary: summary}, nil
}

// ExportLogs calls fn for every log matching f, oldest first. Page fields are
// ignored. Rows are read in bounded batches that are closed before fn is
// called, so a slow client download does not hold the single SQLite
// connection and stall the gateway. The newest id is captured up front so
// requests logged during a long download do not extend the export.
func (s *Store) ExportLogs(f LogFilter, fn func(*Log) error) error {
	const batchSize = 500
	where, baseArgs := logWhere(f)
	var maxID int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM logs`+where, baseArgs...).Scan(&maxID); err != nil {
		return err
	}
	if maxID == 0 {
		return nil
	}
	lastID := int64(0)
	for {
		clause := ` WHERE id > ? AND id <= ?`
		if where != "" {
			clause = where + ` AND id > ? AND id <= ?`
		}
		args := append(append([]any{}, baseArgs...), lastID, maxID, batchSize)
		rows, err := s.db.Query(`SELECT `+logColumns+` FROM logs`+clause+` ORDER BY id ASC LIMIT ?`, args...)
		if err != nil {
			return err
		}
		batch := make([]Log, 0, batchSize)
		for rows.Next() {
			l, err := scanLog(rows)
			if err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, *l)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for i := range batch {
			if err := fn(&batch[i]); err != nil {
				return err
			}
			lastID = batch[i].ID
		}
		if len(batch) < batchSize {
			return nil
		}
	}
}

func scanLog(row interface{ Scan(dest ...any) error }) (*Log, error) {
	var l Log
	err := row.Scan(&l.ID, &l.RequestID, &l.ReservationID, &l.UserID, &l.TokenID, &l.ChannelID, &l.TokenName, &l.ChannelName, &l.Model, &l.UpstreamModel,
		&l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.Cost, &l.ReservedCost, &l.CostStatus, &l.Status, &l.StatusCode, &l.Error, &l.LatencyMS, &l.Stream, &l.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// VideoTask is one asynchronous video generation task.
type VideoTask struct {
	ID             int64  `json:"id"`
	UpstreamTaskID string `json:"upstream_task_id"`
	ChannelID      int64  `json:"channel_id"`
	ChannelName    string `json:"channel_name"`
	Model          string `json:"model"`
	UpstreamModel  string `json:"upstream_model"`
	Prompt         string `json:"prompt"`
	Mode           string `json:"mode"`
	Resolution     string `json:"resolution"`
	Duration       int    `json:"duration"`
	Ratio          string `json:"ratio"`
	Status         string `json:"status"`
	Error          string `json:"error"`
	VideoPath      string `json:"-"`
	RemoteURL      string `json:"-"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

const videoTaskColumns = `id, upstream_task_id, channel_id, channel_name, model, upstream_model, prompt, mode, resolution, duration, ratio, status, error, video_path, remote_url, created_at, updated_at`

// CreateVideoTask inserts a video task and sets its id.
func (s *Store) CreateVideoTask(v *VideoTask) error {
	now := now()
	v.CreatedAt, v.UpdatedAt = now, now
	var err error
	v.ID, err = s.insertStoreID(`INSERT INTO video_tasks (upstream_task_id, channel_id, channel_name, model, upstream_model, prompt, mode,
		resolution, duration, ratio, status, error, video_path, remote_url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.UpstreamTaskID, v.ChannelID, v.ChannelName, v.Model, v.UpstreamModel, v.Prompt, v.Mode,
		v.Resolution, v.Duration, v.Ratio, v.Status, v.Error, v.VideoPath, v.RemoteURL, v.CreatedAt, v.UpdatedAt)
	return err
}

// UpdateVideoTask persists the mutable state of a video task.
func (s *Store) UpdateVideoTask(v *VideoTask) error {
	v.UpdatedAt = now()
	_, err := s.db.Exec(`UPDATE video_tasks SET status=?, error=?, video_path=?, remote_url=?, resolution=?, duration=?, ratio=?, updated_at=? WHERE id=?`,
		v.Status, v.Error, v.VideoPath, v.RemoteURL, v.Resolution, v.Duration, v.Ratio, v.UpdatedAt, v.ID)
	return err
}

// GetVideoTask returns a video task by id.
func (s *Store) GetVideoTask(id int64) (*VideoTask, error) {
	row := s.db.QueryRow(`SELECT `+videoTaskColumns+` FROM video_tasks WHERE id = ?`, id)
	v, err := scanVideoTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

// ListVideoTasks returns one page of video tasks, newest first, with the total count.
func (s *Store) ListVideoTasks(page, pageSize int) ([]VideoTask, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM video_tasks`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT `+videoTaskColumns+` FROM video_tasks ORDER BY id DESC LIMIT ? OFFSET ?`,
		pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]VideoTask, 0)
	for rows.Next() {
		v, err := scanVideoTask(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *v)
	}
	return out, total, rows.Err()
}

// ListResumableVideoTasks returns tasks whose upstream work should still be polled
// or whose generated video still has to be downloaded.
func (s *Store) ListResumableVideoTasks() ([]VideoTask, error) {
	rows, err := s.db.Query(`SELECT ` + videoTaskColumns + ` FROM video_tasks
		WHERE status IN ('queued', 'running') OR (status = 'succeeded' AND video_path = '')
		ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]VideoTask, 0)
	for rows.Next() {
		v, err := scanVideoTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

func scanVideoTask(row interface{ Scan(dest ...any) error }) (*VideoTask, error) {
	var v VideoTask
	err := row.Scan(&v.ID, &v.UpstreamTaskID, &v.ChannelID, &v.ChannelName, &v.Model, &v.UpstreamModel, &v.Prompt, &v.Mode,
		&v.Resolution, &v.Duration, &v.Ratio, &v.Status, &v.Error, &v.VideoPath, &v.RemoteURL, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// SpeechClip is one MiniMax text-to-speech synthesis. Short text finishes as a
// single request. Long text is submitted once to MiniMax's asynchronous
// long-text endpoint and tracked by UpstreamTaskID until its audio is ready.
// Clips created by the older chunked implementation have no UpstreamTaskID and
// keep their TotalChunks/DoneChunks progress.
type SpeechClip struct {
	ID             int64   `json:"id"`
	ChannelID      int64   `json:"channel_id"`
	ChannelName    string  `json:"channel_name"`
	Model          string  `json:"model"`
	UpstreamModel  string  `json:"upstream_model"`
	VoiceID        string  `json:"voice_id"`
	VoiceName      string  `json:"voice_name"`
	Text           string  `json:"text"`
	Speed          float64 `json:"speed"`
	Status         string  `json:"status"`
	Error          string  `json:"error"`
	TotalChunks    int     `json:"total_chunks"`
	DoneChunks     int     `json:"done_chunks"`
	UpstreamTaskID string  `json:"-"`
	UpstreamFileID int64   `json:"-"`
	Format         string  `json:"format"`
	AudioPath      string  `json:"-"`
	DurationMS     int64   `json:"duration_ms"`
	Characters     int64   `json:"characters"`
	SizeBytes      int64   `json:"size_bytes"`
	CreatedAt      int64   `json:"created_at"`
}

const speechClipColumns = `id, channel_id, channel_name, model, upstream_model, voice_id, voice_name, text, speed,
	status, error, total_chunks, done_chunks, upstream_task_id, upstream_file_id, format, audio_path, duration_ms, characters, size_bytes, created_at`

// CreateSpeechClip inserts a generated speech clip or background job and sets
// its id. An empty status means the clip is already complete.
func (s *Store) CreateSpeechClip(c *SpeechClip) error {
	if c.Status == "" {
		c.Status = "succeeded"
	}
	if c.Speed == 0 {
		c.Speed = 1
	}
	c.CreatedAt = now()
	var err error
	c.ID, err = s.insertStoreID(`INSERT INTO speech_clips (channel_id, channel_name, model, upstream_model, voice_id, voice_name,
		text, speed, status, error, total_chunks, done_chunks, upstream_task_id, upstream_file_id, format, audio_path,
		duration_ms, characters, size_bytes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ChannelID, c.ChannelName, c.Model, c.UpstreamModel, c.VoiceID, c.VoiceName,
		c.Text, c.Speed, c.Status, c.Error, c.TotalChunks, c.DoneChunks, c.UpstreamTaskID, c.UpstreamFileID,
		c.Format, c.AudioPath, c.DurationMS, c.Characters, c.SizeBytes, c.CreatedAt)
	return err
}

// UpdateSpeechClip persists the mutable state of a background speech job while
// it advances, including partial progress and the final audio path.
func (s *Store) UpdateSpeechClip(c *SpeechClip) error {
	_, err := s.db.Exec(`UPDATE speech_clips SET channel_id=?, channel_name=?, upstream_model=?, status=?, error=?,
		total_chunks=?, done_chunks=?, upstream_task_id=?, upstream_file_id=?, format=?, audio_path=?, duration_ms=?,
		characters=?, size_bytes=? WHERE id=?`,
		c.ChannelID, c.ChannelName, c.UpstreamModel, c.Status, c.Error, c.TotalChunks, c.DoneChunks,
		c.UpstreamTaskID, c.UpstreamFileID, c.Format, c.AudioPath, c.DurationMS, c.Characters, c.SizeBytes, c.ID)
	return err
}

// GetSpeechClip returns a speech clip by id.
func (s *Store) GetSpeechClip(id int64) (*SpeechClip, error) {
	row := s.db.QueryRow(`SELECT `+speechClipColumns+` FROM speech_clips WHERE id = ?`, id)
	c, err := scanSpeechClip(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ListSpeechClips returns one page of speech clips, newest first, with the total count.
func (s *Store) ListSpeechClips(page, pageSize int) ([]SpeechClip, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM speech_clips`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT `+speechClipColumns+` FROM speech_clips ORDER BY id DESC LIMIT ? OFFSET ?`,
		pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]SpeechClip, 0)
	for rows.Next() {
		c, err := scanSpeechClip(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *c)
	}
	return out, total, rows.Err()
}

// ListResumableSpeechClips returns background jobs that were still queued or
// running when the gateway stopped, so they can continue after a restart.
func (s *Store) ListResumableSpeechClips() ([]SpeechClip, error) {
	rows, err := s.db.Query(`SELECT ` + speechClipColumns + ` FROM speech_clips
		WHERE status IN ('queued', 'running') ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SpeechClip, 0)
	for rows.Next() {
		c, err := scanSpeechClip(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func scanSpeechClip(row interface{ Scan(dest ...any) error }) (*SpeechClip, error) {
	var c SpeechClip
	err := row.Scan(&c.ID, &c.ChannelID, &c.ChannelName, &c.Model, &c.UpstreamModel, &c.VoiceID, &c.VoiceName,
		&c.Text, &c.Speed, &c.Status, &c.Error, &c.TotalChunks, &c.DoneChunks, &c.UpstreamTaskID, &c.UpstreamFileID,
		&c.Format, &c.AudioPath, &c.DurationMS, &c.Characters, &c.SizeBytes, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// VoiceClone is one custom voice cloned from an authorized sample recording.
// The provider owns the voice; the relay keeps the metadata and a locally
// stored preview so the console can replay it after the temporary provider URL
// expires.
type VoiceClone struct {
	ID                int64  `json:"id"`
	VoiceID           string `json:"voice_id"`
	Name              string `json:"name"`
	ChannelID         int64  `json:"channel_id"`
	ChannelName       string `json:"channel_name"`
	Model             string `json:"model"`
	UpstreamModel     string `json:"upstream_model"`
	SampleFormat      string `json:"sample_format"`
	SampleBytes       int64  `json:"sample_bytes"`
	SourceFilename    string `json:"source_filename"`
	PreviewText       string `json:"preview_text"`
	PreviewPath       string `json:"-"`
	PreviewFormat     string `json:"preview_format"`
	PreviewDurationMS int64  `json:"preview_duration_ms"`
	PreviewCharacters int64  `json:"preview_characters"`
	Authorized        bool   `json:"authorized"`
	CreatedAt         int64  `json:"created_at"`
}

const voiceCloneColumns = `id, voice_id, name, channel_id, channel_name, model, upstream_model,
	sample_format, sample_bytes, source_filename, preview_text, preview_path, preview_format,
	preview_duration_ms, preview_characters, authorized, created_at`

// CreateVoiceClone inserts a cloned voice and sets its id.
func (s *Store) CreateVoiceClone(c *VoiceClone) error {
	c.CreatedAt = now()
	var err error
	c.ID, err = s.insertStoreID(`INSERT INTO voice_clones (voice_id, name, channel_id, channel_name, model,
		upstream_model, sample_format, sample_bytes, source_filename, preview_text, preview_path,
		preview_format, preview_duration_ms, preview_characters, authorized, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.VoiceID, c.Name, c.ChannelID, c.ChannelName, c.Model, c.UpstreamModel, c.SampleFormat,
		c.SampleBytes, c.SourceFilename, c.PreviewText, c.PreviewPath, c.PreviewFormat,
		c.PreviewDurationMS, c.PreviewCharacters, c.Authorized, c.CreatedAt)
	return err
}

// GetVoiceClone returns a cloned voice by id.
func (s *Store) GetVoiceClone(id int64) (*VoiceClone, error) {
	row := s.db.QueryRow(`SELECT `+voiceCloneColumns+` FROM voice_clones WHERE id = ?`, id)
	c, err := scanVoiceClone(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// GetVoiceCloneByVoiceID returns a cloned voice by its provider voice id.
func (s *Store) GetVoiceCloneByVoiceID(voiceID string) (*VoiceClone, error) {
	row := s.db.QueryRow(`SELECT `+voiceCloneColumns+` FROM voice_clones WHERE voice_id = ?`, voiceID)
	c, err := scanVoiceClone(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ListVoiceClones returns every cloned voice, newest first.
func (s *Store) ListVoiceClones() ([]VoiceClone, error) {
	rows, err := s.db.Query(`SELECT ` + voiceCloneColumns + ` FROM voice_clones ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]VoiceClone, 0)
	for rows.Next() {
		c, err := scanVoiceClone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// UpdateVoiceClonePreview stores the regenerated preview metadata.
func (s *Store) UpdateVoiceClonePreview(c *VoiceClone) error {
	_, err := s.db.Exec(`UPDATE voice_clones SET preview_text = ?, preview_path = ?, preview_format = ?,
		preview_duration_ms = ?, preview_characters = ? WHERE id = ?`,
		c.PreviewText, c.PreviewPath, c.PreviewFormat, c.PreviewDurationMS, c.PreviewCharacters, c.ID)
	return err
}

func scanVoiceClone(row interface{ Scan(dest ...any) error }) (*VoiceClone, error) {
	var c VoiceClone
	err := row.Scan(&c.ID, &c.VoiceID, &c.Name, &c.ChannelID, &c.ChannelName, &c.Model,
		&c.UpstreamModel, &c.SampleFormat, &c.SampleBytes, &c.SourceFilename, &c.PreviewText,
		&c.PreviewPath, &c.PreviewFormat, &c.PreviewDurationMS, &c.PreviewCharacters,
		&c.Authorized, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// VoiceDesign is one custom voice generated from a text description by the
// MiniMax voice-design API. PreviewPath points at the generated preview audio.
type VoiceDesign struct {
	ID                int64  `json:"id"`
	ChannelID         int64  `json:"channel_id"`
	ChannelName       string `json:"channel_name"`
	Model             string `json:"model"`
	UpstreamModel     string `json:"upstream_model"`
	VoiceID           string `json:"voice_id"`
	Name              string `json:"name"`
	Prompt            string `json:"prompt"`
	PreviewText       string `json:"preview_text"`
	PreviewPath       string `json:"-"`
	PreviewFormat     string `json:"preview_format"`
	PreviewDurationMS int64  `json:"preview_duration_ms"`
	PreviewCharacters int64  `json:"preview_characters"`
	PreviewSizeBytes  int64  `json:"preview_size_bytes"`
	CreatedAt         int64  `json:"created_at"`
}

const voiceDesignColumns = `id, channel_id, channel_name, model, upstream_model, voice_id, name, prompt,
	preview_text, preview_path, preview_format, preview_duration_ms, preview_characters, preview_size_bytes, created_at`

// CreateVoiceDesign inserts a designed voice and sets its id.
func (s *Store) CreateVoiceDesign(v *VoiceDesign) error {
	v.CreatedAt = now()
	var err error
	v.ID, err = s.insertStoreID(`INSERT INTO voice_designs (channel_id, channel_name, model, upstream_model, voice_id, name,
		prompt, preview_text, preview_path, preview_format, preview_duration_ms, preview_characters, preview_size_bytes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ChannelID, v.ChannelName, v.Model, v.UpstreamModel, v.VoiceID, v.Name,
		v.Prompt, v.PreviewText, v.PreviewPath, v.PreviewFormat, v.PreviewDurationMS, v.PreviewCharacters, v.PreviewSizeBytes, v.CreatedAt)
	return err
}

// GetVoiceDesign returns a designed voice by id.
func (s *Store) GetVoiceDesign(id int64) (*VoiceDesign, error) {
	row := s.db.QueryRow(`SELECT `+voiceDesignColumns+` FROM voice_designs WHERE id = ?`, id)
	v, err := scanVoiceDesign(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

// ListVoiceDesigns returns up to limit designed voices, newest first.
func (s *Store) ListVoiceDesigns(limit int) ([]VoiceDesign, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+voiceDesignColumns+` FROM voice_designs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]VoiceDesign, 0)
	for rows.Next() {
		v, err := scanVoiceDesign(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// GetVoiceDesignByVoiceID returns the newest designed voice for a provider
// voice id. Designed voices are account-scoped, so callers use this to pin
// synthesis to the channel that created the voice.
func (s *Store) GetVoiceDesignByVoiceID(voiceID string) (*VoiceDesign, error) {
	row := s.db.QueryRow(`SELECT `+voiceDesignColumns+` FROM voice_designs WHERE voice_id = ? ORDER BY id DESC LIMIT 1`, voiceID)
	v, err := scanVoiceDesign(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func scanVoiceDesign(row interface{ Scan(dest ...any) error }) (*VoiceDesign, error) {
	var v VoiceDesign
	err := row.Scan(&v.ID, &v.ChannelID, &v.ChannelName, &v.Model, &v.UpstreamModel, &v.VoiceID, &v.Name, &v.Prompt,
		&v.PreviewText, &v.PreviewPath, &v.PreviewFormat, &v.PreviewDurationMS, &v.PreviewCharacters, &v.PreviewSizeBytes, &v.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// MusicTask is one music generation task. Unlike video tasks, MiniMax Music
// generation is synchronous, so the gateway keeps the state itself and runs the
// upstream call in the background.
type MusicTask struct {
	ID            int64  `json:"id"`
	ChannelID     int64  `json:"channel_id"`
	ChannelName   string `json:"channel_name"`
	Model         string `json:"model"`
	UpstreamModel string `json:"upstream_model"`
	Prompt        string `json:"prompt"`
	Lyrics        string `json:"lyrics"`
	Instrumental  bool   `json:"instrumental"`
	Format        string `json:"format"`
	Status        string `json:"status"`
	Error         string `json:"error"`
	AudioPath     string `json:"-"`
	RemoteURL     string `json:"-"`
	DurationMS    int64  `json:"duration_ms"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

const musicTaskColumns = `id, channel_id, channel_name, model, upstream_model, prompt, lyrics, instrumental, format,
	status, error, audio_path, remote_url, duration_ms, created_at, updated_at`

// CreateMusicTask inserts a music task and sets its id.
func (s *Store) CreateMusicTask(m *MusicTask) error {
	now := now()
	m.CreatedAt, m.UpdatedAt = now, now
	var err error
	m.ID, err = s.insertStoreID(`INSERT INTO music_tasks (channel_id, channel_name, model, upstream_model, prompt, lyrics,
		instrumental, format, status, error, audio_path, remote_url, duration_ms, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ChannelID, m.ChannelName, m.Model, m.UpstreamModel, m.Prompt, m.Lyrics,
		boolValue(m.Instrumental), m.Format, m.Status, m.Error, m.AudioPath, m.RemoteURL, m.DurationMS, m.CreatedAt, m.UpdatedAt)
	return err
}

// UpdateMusicTask persists the mutable state of a music task.
func (s *Store) UpdateMusicTask(m *MusicTask) error {
	m.UpdatedAt = now()
	_, err := s.db.Exec(`UPDATE music_tasks SET status=?, error=?, audio_path=?, remote_url=?, duration_ms=?, updated_at=? WHERE id=?`,
		m.Status, m.Error, m.AudioPath, m.RemoteURL, m.DurationMS, m.UpdatedAt, m.ID)
	return err
}

// GetMusicTask returns a music task by id.
func (s *Store) GetMusicTask(id int64) (*MusicTask, error) {
	row := s.db.QueryRow(`SELECT `+musicTaskColumns+` FROM music_tasks WHERE id = ?`, id)
	m, err := scanMusicTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// ListMusicTasks returns one page of music tasks, newest first, with the total count.
func (s *Store) ListMusicTasks(page, pageSize int) ([]MusicTask, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM music_tasks`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT `+musicTaskColumns+` FROM music_tasks ORDER BY id DESC LIMIT ? OFFSET ?`,
		pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]MusicTask, 0)
	for rows.Next() {
		m, err := scanMusicTask(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *m)
	}
	return out, total, rows.Err()
}

// ListInterruptedMusicTasks returns generation tasks that were still running
// when the process stopped. Their synchronous upstream call cannot be resumed,
// so startup marks them failed instead of leaving the console polling forever.
func (s *Store) ListInterruptedMusicTasks() ([]MusicTask, error) {
	rows, err := s.db.Query(`SELECT ` + musicTaskColumns + ` FROM music_tasks
		WHERE status IN ('queued', 'generating') ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MusicTask, 0)
	for rows.Next() {
		m, err := scanMusicTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func scanMusicTask(row interface{ Scan(dest ...any) error }) (*MusicTask, error) {
	var m MusicTask
	var instrumental int64
	err := row.Scan(&m.ID, &m.ChannelID, &m.ChannelName, &m.Model, &m.UpstreamModel, &m.Prompt, &m.Lyrics,
		&instrumental, &m.Format, &m.Status, &m.Error, &m.AudioPath, &m.RemoteURL, &m.DurationMS, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	m.Instrumental = instrumental != 0
	return &m, nil
}

// Totals aggregates request counts, token usage and spend.
type Totals struct {
	Requests         int64   `json:"requests"`
	Success          int64   `json:"success"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Cost             float64 `json:"cost"`
	UnverifiedCost   float64 `json:"unverified_cost"`
}

// DayStat is one day of traffic.
type DayStat struct {
	Date     string  `json:"date"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
	Errors   int64   `json:"errors"`
}

// ModelStat aggregates traffic per model.
type ModelStat struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// Dashboard is the admin overview payload.
type Dashboard struct {
	Totals     Totals      `json:"totals"`
	Today      Totals      `json:"today"`
	Series     []DayStat   `json:"series"`
	TopModels  []ModelStat `json:"top_models"`
	RecentLogs []Log       `json:"recent_logs"`
	Channels   CountPair   `json:"channels"`
	Tokens     CountPair   `json:"tokens"`
}

// CountPair reports total and active counts.
type CountPair struct {
	Total  int64 `json:"total"`
	Active int64 `json:"active"`
}

// GetDashboard builds the admin overview.
func (s *Store) GetDashboard() (*Dashboard, error) {
	d := &Dashboard{
		Series:     make([]DayStat, 0, 7),
		TopModels:  make([]ModelStat, 0),
		RecentLogs: make([]Log, 0),
	}
	var err error
	if d.Totals, err = s.totalsSince(0); err != nil {
		return nil, err
	}
	nowTime := time.Now().UTC()
	todayStart := time.Date(nowTime.Year(), nowTime.Month(), nowTime.Day(), 0, 0, 0, 0, time.UTC).Unix()
	if d.Today, err = s.totalsSince(todayStart); err != nil {
		return nil, err
	}
	start := todayStart - 6*86400
	byDay := map[string]*DayStat{}
	// SQLite stores unix seconds and formats them with date(); PostgreSQL
	// needs an explicit UTC conversion so the dashboard keeps UTC day buckets.
	dayExpr := `date(created_at, 'unixepoch')`
	if s.dialect == dialectPostgres {
		dayExpr = `to_char(to_timestamp(created_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD')`
	}
	rows, err := s.db.Query(`SELECT `+dayExpr+`, COUNT(*), COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(cost), 0), COALESCE(SUM(CASE WHEN status = 'success' THEN 0 ELSE 1 END), 0)
		FROM logs WHERE created_at >= ? GROUP BY `+dayExpr, start)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ds DayStat
		if err := rows.Scan(&ds.Date, &ds.Requests, &ds.Tokens, &ds.Cost, &ds.Errors); err != nil {
			rows.Close()
			return nil, err
		}
		byDay[ds.Date] = &ds
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := 0; i < 7; i++ {
		day := time.Unix(start+int64(i)*86400, 0).UTC().Format("2006-01-02")
		if ds, ok := byDay[day]; ok {
			d.Series = append(d.Series, *ds)
		} else {
			d.Series = append(d.Series, DayStat{Date: day})
		}
	}
	modelRows, err := s.db.Query(`SELECT model, COUNT(*), COALESCE(SUM(total_tokens), 0) FROM logs
		WHERE created_at >= ? AND status = 'success' GROUP BY model ORDER BY COUNT(*) DESC LIMIT 5`, start)
	if err != nil {
		return nil, err
	}
	for modelRows.Next() {
		var ms ModelStat
		if err := modelRows.Scan(&ms.Model, &ms.Requests, &ms.Tokens); err != nil {
			modelRows.Close()
			return nil, err
		}
		d.TopModels = append(d.TopModels, ms)
	}
	modelRows.Close()
	if err := modelRows.Err(); err != nil {
		return nil, err
	}
	logs, _, err := s.ListLogs(LogFilter{Page: 1, PageSize: 8})
	if err != nil {
		return nil, err
	}
	d.RecentLogs = logs
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN status = 'enabled' THEN 1 ELSE 0 END), 0) FROM channels`).
		Scan(&d.Channels.Total, &d.Channels.Active); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END), 0) FROM tokens`).
		Scan(&d.Tokens.Total, &d.Tokens.Active); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Store) totalsSince(start int64) (Totals, error) {
	var t Totals
	query := `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'success' THEN 0 ELSE 1 END), 0),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(cost), 0),
		COALESCE(SUM(CASE WHEN cost_status = 'unverified' THEN cost ELSE 0 END), 0)
		FROM logs`
	var err error
	if start > 0 {
		err = s.db.QueryRow(query+` WHERE created_at >= ?`, start).Scan(&t.Requests, &t.Success, &t.Errors, &t.PromptTokens, &t.CompletionTokens, &t.TotalTokens, &t.Cost, &t.UnverifiedCost)
	} else {
		err = s.db.QueryRow(query).Scan(&t.Requests, &t.Success, &t.Errors, &t.PromptTokens, &t.CompletionTokens, &t.TotalTokens, &t.Cost, &t.UnverifiedCost)
	}
	return t, err
}

func now() int64 { return time.Now().Unix() }

func encodeStrings(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeStrings(s string) []string {
	var v []string
	if err := json.Unmarshal([]byte(s), &v); err != nil || v == nil {
		return []string{}
	}
	return v
}

func encodeMap(v map[string]string) string {
	if v == nil {
		v = map[string]string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeMap(s string) map[string]string {
	var v map[string]string
	if err := json.Unmarshal([]byte(s), &v); err != nil || v == nil {
		return map[string]string{}
	}
	return v
}

func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}
