package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Reservation states. A reservation is pending while its request is in flight,
// then becomes settled, released (no charge) or recovered (process restart
// found it still pending).
const (
	ReservationPending   = "pending"
	ReservationSettled   = "settled"
	ReservationReleased  = "released"
	ReservationRecovered = "recovered"
)

// Cost statuses recorded next to a charge.
const (
	// CostActual is computed from usage reported by the upstream.
	CostActual = "actual"
	// CostEstimated is computed from locally counted content because the
	// upstream omitted usage.
	CostEstimated = "estimated"
	// CostUnverified marks a charge whose real usage is unknown. The reserved
	// upper bound is charged and left visible for later verification.
	CostUnverified = "unverified"
	// CostFree means the request was rejected before upstream usage was
	// possible, or a pre-response upstream failure was confirmed not to bill.
	CostFree = "free"
)

// Reservation is one quota hold created before a request reaches an upstream.
type Reservation struct {
	ID               string  `json:"id"`
	TokenID          int64   `json:"token_id"`
	TokenName        string  `json:"token_name"`
	RequestID        string  `json:"request_id"`
	Status           string  `json:"status"`
	ReservedCost     float64 `json:"reserved_cost"`
	SettledCost      float64 `json:"settled_cost"`
	CostStatus       string  `json:"cost_status"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Model            string  `json:"model"`
	UpstreamModel    string  `json:"upstream_model"`
	ChannelID        int64   `json:"channel_id"`
	ChannelName      string  `json:"channel_name"`
	Stream           bool    `json:"stream"`
	CreatedAt        int64   `json:"created_at"`
	SettledAt        *int64  `json:"settled_at"`
	ExpiresAt        int64   `json:"expires_at"`
}

const reservationColumns = `id, token_id, token_name, request_id, status, reserved_cost, settled_cost, cost_status,
	prompt_tokens, completion_tokens, model, upstream_model, channel_id, channel_name, stream, created_at, settled_at, expires_at`

// ReserveQuota creates an atomic hold against a key. The available balance is
// quota - used_quota - SUM(pending reservations), so concurrent requests for
// the same key can never spend the same balance twice. Unlimited keys
// (quota < 0) always succeed but still get a reservation for accounting and
// restart recovery.
func (s *Store) ReserveQuota(id string, tokenID int64, requestID, model string, amount float64, expiresAt int64) (*Reservation, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var quota, used float64
	var status string
	var expires sql.NullInt64
	if err := tx.QueryRow(`SELECT quota, used_quota, status, expires_at FROM tokens WHERE id = ?`, tokenID).
		Scan(&quota, &used, &status, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if status != "active" {
		return nil, ErrTokenInactive
	}
	if expires.Valid && expires.Int64 <= time.Now().Unix() {
		return nil, ErrTokenExpired
	}
	var pending float64
	if err := tx.QueryRow(`SELECT COALESCE(SUM(reserved_cost), 0) FROM token_reservations WHERE token_id = ? AND status = ?`,
		tokenID, ReservationPending).Scan(&pending); err != nil {
		return nil, err
	}
	if quota >= 0 {
		available := quota - used - pending
		if available < 0 {
			available = 0
		}
		if amount > available+1e-9 {
			return nil, &InsufficientQuotaError{Required: amount, Available: available, Quota: quota, Used: used, Reserved: pending}
		}
	}
	now := now()
	var tokenName string
	if err := tx.QueryRow(`SELECT name FROM tokens WHERE id = ?`, tokenID).Scan(&tokenName); err != nil {
		return nil, err
	}
	res := &Reservation{
		ID:           id,
		TokenID:      tokenID,
		TokenName:    tokenName,
		RequestID:    requestID,
		Status:       ReservationPending,
		ReservedCost: amount,
		CostStatus:   "pending",
		Model:        model,
		CreatedAt:    now,
		ExpiresAt:    expiresAt,
	}
	if _, err := tx.Exec(`INSERT INTO token_reservations (id, token_id, token_name, request_id, status, reserved_cost, settled_cost, cost_status,
		prompt_tokens, completion_tokens, model, upstream_model, channel_id, channel_name, stream, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, 'pending', 0, 0, ?, '', 0, '', 0, ?, ?)`,
		res.ID, res.TokenID, res.TokenName, res.RequestID, res.Status, res.ReservedCost, res.Model, res.CreatedAt, res.ExpiresAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

// FinalizeReservation settles a pending reservation exactly once and writes the
// matching request log in the same transaction. Releasing records a free
// request instead of a charge. It reports false when the reservation was
// already finalized (for example a duplicate completion notification), in
// which case neither the key balance nor the logs change.
func (s *Store) FinalizeReservation(id string, log *Log, cost float64, costStatus string, release bool) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	settledAt := now()
	status := ReservationSettled
	if release {
		status = ReservationReleased
		cost = 0
		costStatus = CostFree
		log.Cost = 0
		log.PromptTokens = 0
		log.CompletionTokens = 0
		log.TotalTokens = 0
	}
	if log.CreatedAt == 0 {
		log.CreatedAt = settledAt
	}
	log.ReservationID = id
	log.Cost = cost
	log.CostStatus = costStatus
	res, err := tx.Exec(`UPDATE token_reservations SET status = ?, settled_cost = ?, cost_status = ?, prompt_tokens = ?, completion_tokens = ?,
		upstream_model = ?, channel_id = ?, channel_name = ?, stream = ?, settled_at = ? WHERE id = ? AND status = ?`,
		status, cost, costStatus, log.PromptTokens, log.CompletionTokens, log.UpstreamModel, log.ChannelID, log.ChannelName, log.Stream, settledAt, id, ReservationPending)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	if cost > 0 {
		if _, err := tx.Exec(`UPDATE tokens SET used_quota = used_quota + ?, last_used_at = ? WHERE id = ?`, cost, settledAt, log.TokenID); err != nil {
			return false, err
		}
	} else {
		if _, err := tx.Exec(`UPDATE tokens SET last_used_at = ? WHERE id = ?`, settledAt, log.TokenID); err != nil {
			return false, err
		}
	}
	if err := insertLogTx(tx, log); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// RecoverPendingReservations finalizes reservations left in flight by a
// previous process. Because this gateway runs as a single node, a pending
// reservation at startup cannot still be running. Since its real usage is
// unknown, it is charged at the reserved upper bound and marked "unverified"
// instead of being released for free or silently kept forever. Each recovered
// request gets a visible log entry so the charge can be reviewed.
func (s *Store) RecoverPendingReservations() ([]Reservation, error) {
	rows, err := s.db.Query(`SELECT `+reservationColumns+` FROM token_reservations WHERE status = ? ORDER BY created_at ASC`, ReservationPending)
	if err != nil {
		return nil, err
	}
	pending := make([]Reservation, 0)
	for rows.Next() {
		res, err := scanReservation(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		pending = append(pending, *res)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	recovered := make([]Reservation, 0, len(pending))
	for i := range pending {
		res := pending[i]
		tx, err := s.db.Begin()
		if err != nil {
			return recovered, err
		}
		settledAt := now()
		update, err := tx.Exec(`UPDATE token_reservations SET status = ?, settled_cost = ?, cost_status = ?, settled_at = ? WHERE id = ? AND status = ?`,
			ReservationRecovered, res.ReservedCost, CostUnverified, settledAt, res.ID, ReservationPending)
		if err != nil {
			_ = tx.Rollback()
			return recovered, err
		}
		affected, err := update.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return recovered, err
		}
		if affected == 0 {
			_ = tx.Rollback()
			continue
		}
		if res.ReservedCost > 0 {
			if _, err := tx.Exec(`UPDATE tokens SET used_quota = used_quota + ?, last_used_at = ? WHERE id = ?`, res.ReservedCost, settledAt, res.TokenID); err != nil {
				_ = tx.Rollback()
				return recovered, err
			}
		}
		latency := (settledAt - res.CreatedAt) * 1000
		if latency < 0 {
			latency = 0
		}
		log := &Log{
			RequestID:     res.RequestID,
			ReservationID: res.ID,
			TokenID:       res.TokenID,
			TokenName:     res.TokenName,
			ChannelID:     res.ChannelID,
			ChannelName:   res.ChannelName,
			Model:         res.Model,
			UpstreamModel: res.UpstreamModel,
			Cost:          res.ReservedCost,
			ReservedCost:  res.ReservedCost,
			CostStatus:    CostUnverified,
			Status:        "error",
			StatusCode:    0,
			Error:         "gateway restarted before this request completed; charged the reserved upper bound and marked the charge unverified",
			LatencyMS:     latency,
			Stream:        res.Stream,
			CreatedAt:     res.CreatedAt,
		}
		if err := insertLogTx(tx, log); err != nil {
			_ = tx.Rollback()
			return recovered, err
		}
		if err := tx.Commit(); err != nil {
			return recovered, err
		}
		res.Status = ReservationRecovered
		res.SettledCost = res.ReservedCost
		res.CostStatus = CostUnverified
		at := settledAt
		res.SettledAt = &at
		recovered = append(recovered, res)
	}
	return recovered, nil
}

// ListReservations returns the most recent reservations for the admin ledger.
func (s *Store) ListReservations(limit int) ([]Reservation, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT `+reservationColumns+` FROM token_reservations ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Reservation, 0)
	for rows.Next() {
		res, err := scanReservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *res)
	}
	return out, rows.Err()
}

func scanReservation(row interface{ Scan(dest ...any) error }) (*Reservation, error) {
	var r Reservation
	var settled sql.NullInt64
	err := row.Scan(&r.ID, &r.TokenID, &r.TokenName, &r.RequestID, &r.Status, &r.ReservedCost, &r.SettledCost, &r.CostStatus,
		&r.PromptTokens, &r.CompletionTokens, &r.Model, &r.UpstreamModel, &r.ChannelID, &r.ChannelName, &r.Stream, &r.CreatedAt, &settled, &r.ExpiresAt)
	if err != nil {
		return nil, err
	}
	if settled.Valid {
		v := settled.Int64
		r.SettledAt = &v
	}
	return &r, nil
}

func insertLogTx(tx *sql.Tx, l *Log) error {
	if l.CreatedAt == 0 {
		l.CreatedAt = now()
	}
	if l.CostStatus == "" {
		l.CostStatus = CostActual
	}
	res, err := tx.Exec(`INSERT INTO logs (request_id, reservation_id, user_id, token_id, channel_id, token_name, channel_name, model, upstream_model,
		prompt_tokens, completion_tokens, total_tokens, cost, reserved_cost, cost_status, status, status_code, error, latency_ms, stream, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.RequestID, l.ReservationID, l.UserID, l.TokenID, l.ChannelID, l.TokenName, l.ChannelName, l.Model, l.UpstreamModel,
		l.PromptTokens, l.CompletionTokens, l.TotalTokens, l.Cost, l.ReservedCost, l.CostStatus, l.Status, l.StatusCode, l.Error, l.LatencyMS, l.Stream, l.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert request log: %w", err)
	}
	l.ID, err = res.LastInsertId()
	return err
}
