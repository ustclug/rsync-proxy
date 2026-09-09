// Package state coordinates admission across generations of one local proxy.
// The database contains runtime state, not rsync payloads or durable history.
package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // Register the CGo-free database/sql driver.
)

const Protocol = 1

const (
	Starting = "starting"
	Serving  = "serving"
	Draining = "draining"
	Exited   = "exited"
)

var (
	ErrPerIP      = errors.New("per-IP connection limit reached")
	ErrFull       = errors.New("upstream queue is full")
	ErrNotCurrent = errors.New("generation is no longer current")
)

type Limit struct {
	Name                  string
	Active, Queued, PerIP int
}

type Generation struct {
	ID       string          `json:"id"`
	PID      int             `json:"pid"`
	Version  string          `json:"version"`
	Control  string          `json:"control"`
	State    string          `json:"state"`
	Deadline int64           `json:"deadline,omitempty"`
	Snapshot json.RawMessage `json:"-"`
}

type Ticket struct {
	ID                 int64
	Active             bool
	Index, Queued, Max int
}

type QueueInfo struct {
	Limit
	ActiveCount, QueuedCount int
}

type Store struct {
	db *sql.DB
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(dir, "state.db")}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(2000)")
	q.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	err = s.write(context.Background(), func(c *sql.Conn) error {
		var version int
		if err := c.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		if version != 0 && version != Protocol {
			return fmt.Errorf("incompatible state schema %d (supported %d)", version, Protocol)
		}
		if version == Protocol {
			return nil
		}
		_, err := c.ExecContext(context.Background(), `
CREATE TABLE generations(id TEXT PRIMARY KEY, pid INTEGER NOT NULL, version TEXT NOT NULL, control TEXT NOT NULL, state TEXT NOT NULL, deadline INTEGER NOT NULL DEFAULT 0, snapshot BLOB);
CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO settings VALUES('current','');
CREATE TABLE limits(name TEXT PRIMARY KEY, active INTEGER NOT NULL, queued INTEGER NOT NULL, per_ip INTEGER NOT NULL);
CREATE TABLE tickets(id INTEGER PRIMARY KEY AUTOINCREMENT, generation TEXT NOT NULL REFERENCES generations(id), upstream TEXT NOT NULL REFERENCES limits(name), ip TEXT NOT NULL, active INTEGER NOT NULL);
CREATE INDEX tickets_upstream ON tickets(upstream, active, id);
CREATE INDEX tickets_ip ON tickets(upstream, ip);
CREATE INDEX tickets_generation ON tickets(generation);
PRAGMA user_version=1;`)
		return err
	})
	if err == nil {
		err = os.Chmod(filepath.Join(dir, "state.db"), 0600)
	}
	if err == nil {
		_, err = db.Exec("PRAGMA journal_mode=WAL")
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Every read/modify/write operation starts as a writer. In particular, no
// deferred read transaction may race another process between COUNT and INSERT.
func (s *Store) write(ctx context.Context, f func(*sql.Conn) error) error {
	c, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err = c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() { _, _ = c.ExecContext(context.Background(), "ROLLBACK") }()
	if err = f(c); err != nil {
		return err
	}
	// Once commit starts, do not let request cancellation make its outcome
	// ambiguous to the caller (which must release any successfully issued ticket).
	if err = ctx.Err(); err != nil {
		return err
	}
	_, err = c.ExecContext(context.Background(), "COMMIT")
	return err
}

func (s *Store) Register(g Generation) error {
	_, err := s.db.Exec("INSERT INTO generations(id,pid,version,control,state) VALUES(?,?,?,?,?)", g.ID, g.PID, g.Version, g.Control, "starting")
	return err
}

func (s *Store) Current() (string, error) {
	var id string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key='current'").Scan(&id)
	return id, err
}

func setLimits(ctx context.Context, c *sql.Conn, limits []Limit) error {
	for _, l := range limits {
		if l.Active < 0 || l.Queued < 0 || l.PerIP < 0 {
			return errors.New("negative connection limit")
		}
		_, err := c.ExecContext(ctx, "INSERT INTO limits VALUES(?,?,?,?) ON CONFLICT(name) DO UPDATE SET active=excluded.active, queued=excluded.queued, per_ip=excluded.per_ip", l.Name, l.Active, l.Queued, l.PerIP)
		if err != nil {
			return err
		}
	}
	return promote(ctx, c)
}

// Activate is the commit point of an upgrade. Old generations retain their
// selected targets but consult these shared limits for every admission.
func (s *Store) Activate(id, parent string, limits []Limit) error {
	ctx := context.Background()
	return s.write(ctx, func(c *sql.Conn) error {
		var current string
		if err := c.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='current'").Scan(&current); err != nil {
			return err
		}
		if current != parent {
			return ErrNotCurrent
		}
		if err := setLimits(ctx, c, limits); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE settings SET value=? WHERE key='current'", id); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx, "UPDATE generations SET state='serving' WHERE id=?", id)
		return err
	})
}

func (s *Store) SetLimits(id string, limits []Limit) error {
	ctx := context.Background()
	return s.write(ctx, func(c *sql.Conn) error {
		var current string
		if err := c.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='current'").Scan(&current); err != nil {
			return err
		}
		if current != id {
			return ErrNotCurrent
		}
		return setLimits(ctx, c, limits)
	})
}

func promote(ctx context.Context, c *sql.Conn) error {
	// Rank only queued tickets. Already granted tickets (including grants whose
	// owner hasn't polled yet) always consume capacity.
	_, err := c.ExecContext(ctx, `WITH waiting AS (
 SELECT t.id, l.active AS max, ROW_NUMBER() OVER(PARTITION BY t.upstream ORDER BY t.id) AS position,
 (SELECT COUNT(*) FROM tickets a WHERE a.upstream=t.upstream AND a.active=1) AS occupied
 FROM tickets t JOIN limits l ON l.name=t.upstream WHERE t.active=0
) UPDATE tickets SET active=1 WHERE id IN (SELECT id FROM waiting WHERE max=0 OR position<=max-occupied)`)
	return err
}

func (s *Store) Acquire(ctx context.Context, generation, upstream, ip string) (Ticket, error) {
	var t Ticket
	err := s.write(ctx, func(c *sql.Conn) error {
		var max, queuedMax, perIP, active, queued, ips int
		if err := c.QueryRowContext(ctx, "SELECT active,queued,per_ip FROM limits WHERE name=?", upstream).Scan(&max, &queuedMax, &perIP); err != nil {
			return err
		}
		if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM tickets WHERE upstream=? AND ip=?", upstream, ip).Scan(&ips); err != nil {
			return err
		}
		if perIP > 0 && ips >= perIP {
			return fmt.Errorf("per-IP cap of %d reached: %w", perIP, ErrPerIP)
		}
		if err := promote(ctx, c); err != nil {
			return err
		}
		if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FILTER(WHERE active=1),COUNT(*) FILTER(WHERE active=0) FROM tickets WHERE upstream=?", upstream).Scan(&active, &queued); err != nil {
			return err
		}
		t.Active = max == 0 || (active < max && queued == 0)
		if !t.Active && queuedMax > 0 && queued >= queuedMax {
			return ErrFull
		}
		r, err := c.ExecContext(ctx, "INSERT INTO tickets(generation,upstream,ip,active) VALUES(?,?,?,?)", generation, upstream, ip, t.Active)
		if err != nil {
			return err
		}
		t.ID, err = r.LastInsertId()
		t.Max = max
		t.Index = queued
		t.Queued = queued + 1
		return err
	})
	return t, err
}

func (s *Store) Release(ctx context.Context, generation string, id int64) error {
	return s.write(ctx, func(c *sql.Conn) error {
		if _, err := c.ExecContext(ctx, "DELETE FROM tickets WHERE generation=? AND id=?", generation, id); err != nil {
			return err
		}
		return promote(ctx, c)
	})
}

// Poll reads all of a generation's queued/granted tickets in one operation.
func (s *Store) Poll(generation string) (map[int64]Ticket, error) {
	rows, err := s.db.Query(`WITH ranked AS (
 SELECT t.id,t.generation,t.active,l.active AS max,
 COUNT(*) FILTER(WHERE t.active=0) OVER(PARTITION BY t.upstream ORDER BY t.id ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS position,
 COUNT(*) FILTER(WHERE t.active=0) OVER(PARTITION BY t.upstream) AS queued
 FROM tickets t JOIN limits l ON l.name=t.upstream
) SELECT id,active,max,position,queued FROM ranked WHERE generation=?`, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]Ticket)
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(&t.ID, &t.Active, &t.Max, &t.Index, &t.Queued); err != nil {
			return nil, err
		}
		result[t.ID] = t
	}
	return result, rows.Err()
}

func (s *Store) Queues() ([]QueueInfo, error) {
	rows, err := s.db.Query(`SELECT l.name,l.active,l.queued,l.per_ip,COUNT(t.id) FILTER(WHERE t.active=1),COUNT(t.id) FILTER(WHERE t.active=0) FROM limits l LEFT JOIN tickets t ON t.upstream=l.name GROUP BY l.name ORDER BY l.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []QueueInfo
	for rows.Next() {
		var q QueueInfo
		if err := rows.Scan(&q.Name, &q.Active, &q.Queued, &q.PerIP, &q.ActiveCount, &q.QueuedCount); err != nil {
			return nil, err
		}
		result = append(result, q)
	}
	return result, rows.Err()
}

func (s *Store) Generations() ([]Generation, error) {
	rows, err := s.db.Query("SELECT id,pid,version,control,state,deadline,snapshot FROM generations ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Generation
	for rows.Next() {
		var g Generation
		var snapshot []byte
		if err := rows.Scan(&g.ID, &g.PID, &g.Version, &g.Control, &g.State, &g.Deadline, &snapshot); err != nil {
			return nil, err
		}
		g.Snapshot = json.RawMessage(snapshot)
		result = append(result, g)
	}
	return result, rows.Err()
}

func (s *Store) Publish(id string, snapshot []byte) error {
	_, err := s.db.Exec("UPDATE generations SET snapshot=? WHERE id=?", snapshot, id)
	return err
}

func (s *Store) Drain(id string, deadline time.Time) error {
	var seconds int64
	if !deadline.IsZero() {
		seconds = deadline.Unix()
	}
	_, err := s.db.Exec("UPDATE generations SET state='draining',deadline=? WHERE id=? AND state='serving'", seconds, id)
	return err
}

func (s *Store) Finish(id string) error {
	ctx := context.Background()
	return s.write(ctx, func(c *sql.Conn) error {
		if _, err := c.ExecContext(ctx, "DELETE FROM tickets WHERE generation=?", id); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE generations SET state='exited' WHERE id=?", id); err != nil {
			return err
		}
		return promote(ctx, c)
	})
}

// Reset is only called under the startup lock after confirming all generation
// locks are unheld. It deliberately preserves the schema and database inode.
func (s *Store) Reset() error {
	return s.write(context.Background(), func(c *sql.Conn) error {
		_, err := c.ExecContext(context.Background(), "DELETE FROM tickets; DELETE FROM generations; DELETE FROM limits; UPDATE settings SET value='' WHERE key='current';")
		return err
	})
}
