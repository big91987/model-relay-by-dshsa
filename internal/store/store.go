// Package store persists users, channels, gateway keys and request logs in SQLite.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/auth"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (and migrates) the database at path.
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
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
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
		`CREATE INDEX IF NOT EXISTS idx_logs_request_id ON logs(request_id)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
	}
	return nil
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
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return false, err
	}
	if count > 0 {
		return false, nil
	}
	hash, err := auth.HashPassword(password)
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
	res, err := s.db.Exec(`INSERT INTO channels (name, type, base_url, api_key, api_key_hint, models, model_mapping, priority, weight, status, input_price, output_price, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Type, c.BaseURL, c.APIKeyEncrypted, c.APIKeyHint, encodeStrings(c.Models), encodeMap(c.ModelMapping),
		c.Priority, c.Weight, c.Status, c.InputPrice, c.OutputPrice, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return err
	}
	c.ID, err = res.LastInsertId()
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
	ID            int64    `json:"id"`
	UserID        int64    `json:"user_id"`
	Name          string   `json:"name"`
	KeyHash       string   `json:"-"`
	KeyPrefix     string   `json:"key_prefix"`
	Status        string   `json:"status"`
	Quota         float64  `json:"quota"`
	UsedQuota     float64  `json:"used_quota"`
	AllowedModels []string `json:"allowed_models"`
	RPMLimit      int      `json:"rpm_limit"`
	ExpiresAt     *int64   `json:"expires_at"`
	LastUsedAt    *int64   `json:"last_used_at"`
	CreatedAt     int64    `json:"created_at"`
}

const tokenColumns = `id, user_id, name, key_hash, key_prefix, status, quota, used_quota, allowed_models, rpm_limit, expires_at, last_used_at, created_at`

// ListTokens returns every gateway key.
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
	return out, rows.Err()
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
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.KeyHash, &t.KeyPrefix, &t.Status, &t.Quota, &t.UsedQuota,
		&allowed, &t.RPMLimit, &expires, &lastUsed, &t.CreatedAt)
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
	res, err := s.db.Exec(`INSERT INTO tokens (user_id, name, key_hash, key_prefix, status, quota, used_quota, allowed_models, rpm_limit, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.UserID, t.Name, t.KeyHash, t.KeyPrefix, t.Status, t.Quota, t.UsedQuota, encodeStrings(t.AllowedModels), t.RPMLimit, t.ExpiresAt, t.CreatedAt)
	if err != nil {
		return err
	}
	t.ID, err = res.LastInsertId()
	return err
}

// UpdateToken persists mutable key fields.
func (s *Store) UpdateToken(t *Token) error {
	_, err := s.db.Exec(`UPDATE tokens SET name=?, status=?, quota=?, allowed_models=?, rpm_limit=?, expires_at=? WHERE id=?`,
		t.Name, t.Status, t.Quota, encodeStrings(t.AllowedModels), t.RPMLimit, t.ExpiresAt, t.ID)
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
	res, err := s.db.Exec(`INSERT INTO logs (request_id, user_id, token_id, channel_id, token_name, channel_name, model, upstream_model,
		prompt_tokens, completion_tokens, total_tokens, cost, status, status_code, error, latency_ms, stream, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.RequestID, l.UserID, l.TokenID, l.ChannelID, l.TokenName, l.ChannelName, l.Model, l.UpstreamModel,
		l.PromptTokens, l.CompletionTokens, l.TotalTokens, l.Cost, l.Status, l.StatusCode, l.Error, l.LatencyMS, l.Stream, l.CreatedAt)
	if err != nil {
		return err
	}
	l.ID, err = res.LastInsertId()
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

// ListLogs returns one page of logs and the total number of matches.
func (s *Store) ListLogs(f LogFilter) ([]Log, int64, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 200 {
		f.PageSize = 50
	}
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
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + joinAnd(clauses)
	}
	var total int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	queryArgs := append(append([]any{}, args...), f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := s.db.Query(`SELECT id, request_id, user_id, token_id, channel_id, token_name, channel_name, model, upstream_model,
		prompt_tokens, completion_tokens, total_tokens, cost, status, status_code, error, latency_ms, stream, created_at
		FROM logs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]Log, 0)
	for rows.Next() {
		l, err := scanLog(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *l)
	}
	return out, total, rows.Err()
}

func scanLog(row interface{ Scan(dest ...any) error }) (*Log, error) {
	var l Log
	err := row.Scan(&l.ID, &l.RequestID, &l.UserID, &l.TokenID, &l.ChannelID, &l.TokenName, &l.ChannelName, &l.Model, &l.UpstreamModel,
		&l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.Cost, &l.Status, &l.StatusCode, &l.Error, &l.LatencyMS, &l.Stream, &l.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// VideoTask is one asynchronous MiniMax-H3 generation task.
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
	res, err := s.db.Exec(`INSERT INTO video_tasks (upstream_task_id, channel_id, channel_name, model, upstream_model, prompt, mode,
		resolution, duration, ratio, status, error, video_path, remote_url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.UpstreamTaskID, v.ChannelID, v.ChannelName, v.Model, v.UpstreamModel, v.Prompt, v.Mode,
		v.Resolution, v.Duration, v.Ratio, v.Status, v.Error, v.VideoPath, v.RemoteURL, v.CreatedAt, v.UpdatedAt)
	if err != nil {
		return err
	}
	v.ID, err = res.LastInsertId()
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

// Totals aggregates request counts, token usage and spend.
type Totals struct {
	Requests         int64   `json:"requests"`
	Success          int64   `json:"success"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Cost             float64 `json:"cost"`
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
	rows, err := s.db.Query(`SELECT date(created_at, 'unixepoch'), COUNT(*), COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(cost), 0), COALESCE(SUM(CASE WHEN status = 'success' THEN 0 ELSE 1 END), 0)
		FROM logs WHERE created_at >= ? GROUP BY date(created_at, 'unixepoch')`, start)
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
		COALESCE(SUM(cost), 0)
		FROM logs`
	var err error
	if start > 0 {
		err = s.db.QueryRow(query+` WHERE created_at >= ?`, start).Scan(&t.Requests, &t.Success, &t.Errors, &t.PromptTokens, &t.CompletionTokens, &t.TotalTokens, &t.Cost)
	} else {
		err = s.db.QueryRow(query).Scan(&t.Requests, &t.Success, &t.Errors, &t.PromptTokens, &t.CompletionTokens, &t.TotalTokens, &t.Cost)
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
