package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// dialect identifies the SQL dialect used by a Store.
type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

func (d dialect) String() string {
	if d == dialectPostgres {
		return "postgres"
	}
	return "sqlite"
}

// conn wraps *sql.DB and rewrites SQLite-style "?" placeholders for
// PostgreSQL. Keeping the call sites on "?" lets both backends share one set
// of queries while PostgreSQL still receives its native "$1" parameters.
type conn struct {
	*sql.DB
	dialect dialect
}

func (c *conn) bind(query string) string {
	if c.dialect == dialectPostgres {
		return rebind(query)
	}
	return query
}

func (c *conn) Exec(query string, args ...any) (sql.Result, error) {
	return c.DB.Exec(c.bind(query), args...)
}

func (c *conn) Query(query string, args ...any) (*sql.Rows, error) {
	return c.DB.Query(c.bind(query), args...)
}

func (c *conn) QueryRow(query string, args ...any) *sql.Row {
	return c.DB.QueryRow(c.bind(query), args...)
}

// Begin starts a transaction whose statements also use "?" placeholders.
func (c *conn) Begin() (*tx, error) {
	inner, err := c.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &tx{Tx: inner, dialect: c.dialect}, nil
}

// tx wraps *sql.Tx with the same placeholder handling as conn.
type tx struct {
	*sql.Tx
	dialect dialect
}

func (t *tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.Tx.Exec(bindFor(t.dialect, query), args...)
}

func (t *tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.Tx.Query(bindFor(t.dialect, query), args...)
}

func (t *tx) QueryRow(query string, args ...any) *sql.Row {
	return t.Tx.QueryRow(bindFor(t.dialect, query), args...)
}

func bindFor(d dialect, query string) string {
	if d == dialectPostgres {
		return rebind(query)
	}
	return query
}

// rebind converts "?" placeholders to PostgreSQL "$n" placeholders while
// leaving question marks inside string literals untouched. All queries in this
// package are static except the log filter, which only appends simple
// predicates and never contains a quoted "?".
func rebind(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	inSingle, inDouble := false, false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'' && !inDouble:
			b.WriteByte(c)
			if inSingle && i+1 < len(query) && query[i+1] == '\'' {
				b.WriteByte(query[i+1])
				i++
				continue
			}
			inSingle = !inSingle
		case c == '"' && !inSingle:
			b.WriteByte(c)
			if inDouble && i+1 < len(query) && query[i+1] == '"' {
				b.WriteByte(query[i+1])
				i++
				continue
			}
			inDouble = !inDouble
		case c == '?' && !inSingle && !inDouble:
			n++
			_, _ = fmt.Fprintf(&b, "$%d", n)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// execQueryer is implemented by conn and tx.
type execQueryer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// insertID inserts one row and returns its generated id. SQLite reports it
// through LastInsertId; PostgreSQL requires RETURNING.
func insertID(q execQueryer, d dialect, query string, args ...any) (int64, error) {
	if d == dialectPostgres {
		var id int64
		if err := q.QueryRow(query+" RETURNING id", args...).Scan(&id); err != nil {
			return 0, err
		}
		return id, nil
	}
	res, err := q.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// insertStoreID is the Store-level shorthand for insertID.
func (s *Store) insertStoreID(query string, args ...any) (int64, error) {
	return insertID(s.db, s.dialect, query, args...)
}

// boolValue stores a Go bool in the integer columns shared by both backends.
// PostgreSQL rejects a bool parameter for a BIGINT column, while SQLite
// accepts either representation.
func boolValue(v bool) int {
	if v {
		return 1
	}
	return 0
}
