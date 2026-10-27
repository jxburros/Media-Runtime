// Package store is the SQLite state: jobs, events, idempotency keys, registries and inputs.
// The schema, pragmas and JSON data columns are those of the TypeScript 0.3.0 release, so an
// existing state directory opens unchanged.
package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	c "github.com/jxburros/media-runtime/internal/contracts"
	"github.com/jxburros/media-runtime/internal/jsonv"
	_ "modernc.org/sqlite"
)

type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Store wraps one SQLite connection. A Store returned inside Transaction is bound to it.
type Store struct {
	DB *sql.DB
	tx *sql.Tx
}

const schema = `PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON;
      CREATE TABLE IF NOT EXISTS jobs (ordinal INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT UNIQUE NOT NULL, namespace TEXT NOT NULL, state TEXT NOT NULL, data TEXT NOT NULL);
      CREATE INDEX IF NOT EXISTS jobs_namespace ON jobs(namespace, ordinal);
      CREATE TABLE IF NOT EXISTS events (job_id TEXT NOT NULL REFERENCES jobs(job_id), sequence INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(job_id,sequence));
      CREATE TABLE IF NOT EXISTS idempotency (namespace TEXT NOT NULL, key TEXT NOT NULL, digest TEXT NOT NULL, job_id TEXT NOT NULL REFERENCES jobs(job_id), PRIMARY KEY(namespace,key));
      CREATE TABLE IF NOT EXISTS models (id TEXT PRIMARY KEY, data TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS recipes (id TEXT PRIMARY KEY, data TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS adapters (id TEXT PRIMARY KEY, data TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS inputs (input_id TEXT PRIMARY KEY, namespace TEXT NOT NULL, data TEXT NOT NULL);
      CREATE INDEX IF NOT EXISTS inputs_namespace ON inputs(namespace, input_id);`

func Open(directory string) (*Store, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, err
	}
	file := filepath.Join(directory, "runtime.sqlite")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		return nil, err
	}
	// One connection serializes every statement, as the single-threaded TypeScript service did.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(file, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{DB: db}
	for _, recipe := range c.DefaultRecipes() {
		value := recipe.Clone().Set("digest", c.Hash(recipe))
		if _, err := db.Exec("INSERT OR IGNORE INTO recipes VALUES (?,?)", recipe.Str("recipe_id"), string(jsonv.MustMarshal(value))); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) q() querier {
	if s.tx != nil {
		return s.tx
	}
	return s.DB
}

// Transaction runs fn atomically; a nested call joins the outer transaction.
func (s *Store) Transaction(fn func(tx *Store) error) error {
	if s.tx != nil {
		return fn(s)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	if err := fn(&Store{DB: s.DB, tx: tx}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func decodeObject(text string) *jsonv.Object {
	v, err := jsonv.Parse([]byte(text))
	if err != nil {
		panic(err)
	}
	o, _ := v.(*jsonv.Object)
	return o
}

func (s *Store) rows(query string, args ...any) []*jsonv.Object {
	rows, err := s.q().Query(query, args...)
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	out := []*jsonv.Object{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			panic(err)
		}
		out = append(out, decodeObject(data))
	}
	if err := rows.Err(); err != nil {
		panic(err)
	}
	return out
}

func (s *Store) row(query string, args ...any) *jsonv.Object {
	var data string
	err := s.q().QueryRow(query, args...).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		panic(err)
	}
	return decodeObject(data)
}

func (s *Store) exec(query string, args ...any) {
	if _, err := s.q().Exec(query, args...); err != nil {
		panic(err)
	}
}

func text(v any) string { return string(jsonv.MustMarshal(v)) }

func (s *Store) InsertInput(record *jsonv.Object) {
	s.exec("INSERT INTO inputs(input_id,namespace,data) VALUES (?,?,?)", record.Str("input_id"), record.Str("namespace"), text(record))
}

// Input returns an input record; an empty namespace matches any owner.
func (s *Store) Input(inputID, namespace string) *jsonv.Object {
	if namespace != "" {
		return s.row("SELECT data FROM inputs WHERE input_id=? AND namespace=?", inputID, namespace)
	}
	return s.row("SELECT data FROM inputs WHERE input_id=?", inputID)
}

func (s *Store) Inputs(namespace string) []*jsonv.Object {
	if namespace != "" {
		return s.rows("SELECT data FROM inputs WHERE namespace=? ORDER BY rowid", namespace)
	}
	return s.rows("SELECT data FROM inputs ORDER BY rowid")
}

func (s *Store) DeleteInput(inputID string) { s.exec("DELETE FROM inputs WHERE input_id=?", inputID) }

// All lists jobs in admission order, optionally in one state.
func (s *Store) All(state string) []*jsonv.Object {
	if state != "" {
		return s.rows("SELECT data FROM jobs WHERE state=? ORDER BY ordinal", state)
	}
	return s.rows("SELECT data FROM jobs ORDER BY ordinal")
}

// Get returns a job; an empty namespace matches any owner.
func (s *Store) Get(jobID, namespace string) *jsonv.Object {
	if namespace != "" {
		return s.row("SELECT data FROM jobs WHERE job_id=? AND namespace=?", jobID, namespace)
	}
	return s.row("SELECT data FROM jobs WHERE job_id=?", jobID)
}

// Page is one page of a namespace's jobs with their ordinals.
func (s *Store) Page(namespace string, newest bool, after, limit int) ([]*jsonv.Object, []int64) {
	var rows *sql.Rows
	var err error
	if newest {
		rows, err = s.q().Query("SELECT ordinal,data FROM jobs WHERE namespace=? ORDER BY ordinal DESC LIMIT ?", namespace, limit)
	} else {
		rows, err = s.q().Query("SELECT ordinal,data FROM jobs WHERE namespace=? AND ordinal>? ORDER BY ordinal LIMIT ?", namespace, after, limit)
	}
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	jobs, ordinals := []*jsonv.Object{}, []int64{}
	for rows.Next() {
		var ordinal int64
		var data string
		if err := rows.Scan(&ordinal, &data); err != nil {
			panic(err)
		}
		jobs = append(jobs, decodeObject(data))
		ordinals = append(ordinals, ordinal)
	}
	return jobs, ordinals
}

func (s *Store) Insert(job *jsonv.Object) {
	s.exec("INSERT INTO jobs(job_id,namespace,state,data) VALUES (?,?,?,?)", job.Str("job_id"), job.Str("namespace"), job.Str("state"), text(job))
}

func (s *Store) Save(job *jsonv.Object) {
	job.Set("updated_at", c.Now())
	s.exec("UPDATE jobs SET state=?,data=? WHERE job_id=?", job.Str("state"), text(job), job.Str("job_id"))
}

// Event appends the next sequence of a job's event history.
func (s *Store) Event(job *jsonv.Object, kind string, data any) *jsonv.Object {
	var sequence int64
	if err := s.q().QueryRow("SELECT COALESCE(MAX(sequence),0)+1 AS n FROM events WHERE job_id=?", job.Str("job_id")).Scan(&sequence); err != nil {
		panic(err)
	}
	event := jsonv.O("job_id", job.Str("job_id"), "sequence", float64(sequence), "timestamp", c.Now(), "type", kind, "data", data)
	s.exec("INSERT INTO events VALUES (?,?,?)", job.Str("job_id"), sequence, text(event))
	return event
}

func (s *Store) Events(jobID string, after int64) []*jsonv.Object {
	return s.rows("SELECT data FROM events WHERE job_id=? AND sequence>? ORDER BY sequence", jobID, after)
}

// Finish records a terminal state once; a job that is already terminal is left unchanged.
func (s *Store) Finish(job *jsonv.Object, state string, errorRecord *jsonv.Object) {
	current := s.Get(job.Str("job_id"), "")
	if current == nil || c.Terminal(current.Str("state")) {
		return
	}
	err := s.Transaction(func(tx *Store) error {
		job.Set("state", state)
		job.Set("error", jsonv.Maybe(errorRecord))
		job.Set("stage", nil)
		tx.Save(job)
		if errorRecord != nil {
			tx.Event(job, "error", errorRecord)
		}
		tx.Event(job, "job_state", jsonv.O("state", state))
		return nil
	})
	if err != nil {
		panic(err)
	}
}

// Registry returns one record of models, recipes or adapters, or nil.
func (s *Store) Registry(kind, id string) *jsonv.Object {
	return s.row("SELECT data FROM "+table(kind)+" WHERE id=?", id)
}

func (s *Store) RegistryAll(kind string) []*jsonv.Object {
	return s.rows("SELECT data FROM " + table(kind) + " ORDER BY id")
}

func (s *Store) Register(kind, id string, data *jsonv.Object) {
	s.exec("INSERT INTO "+table(kind)+" VALUES (?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", id, text(data))
}

func table(kind string) string {
	switch kind {
	case "models", "recipes", "adapters":
		return kind
	}
	panic("unknown registry " + kind)
}

// Idempotency returns the digest and job of a stored key.
func (s *Store) Idempotency(namespace, key string) (digest, jobID string, ok bool) {
	err := s.q().QueryRow("SELECT digest, job_id FROM idempotency WHERE namespace=? AND key=?", namespace, key).Scan(&digest, &jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		panic(err)
	}
	return digest, jobID, true
}

func (s *Store) InsertIdempotency(namespace, key, digest, jobID string) {
	s.exec("INSERT INTO idempotency VALUES (?,?,?,?)", namespace, key, digest, jobID)
}
