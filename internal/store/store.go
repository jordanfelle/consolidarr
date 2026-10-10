// Package store is consolidarr's SQLite persistence layer.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Store works on SQLite (default) or Postgres (CONSOLIDARR_DATABASE_URL=postgres://...).
// Queries are written with ? placeholders and rebound for Postgres.
type Store struct {
	DB *sql.DB
	PG bool
}

func (s *Store) rebind(q string) string {
	if !s.PG {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *Store) Exec(q string, a ...any) (sql.Result, error) { return s.DB.Exec(s.rebind(q), a...) }
func (s *Store) Query(q string, a ...any) (*sql.Rows, error) { return s.DB.Query(s.rebind(q), a...) }
func (s *Store) QueryRow(q string, a ...any) *sql.Row        { return s.DB.QueryRow(s.rebind(q), a...) }

const schemaTmpl = `
create table if not exists users(id {{PK}}, username text unique not null, pw_hash text not null);
create table if not exists sessions(token text primary key, user_id bigint not null, expires bigint not null);
create table if not exists instances(
  id {{PK}}, name text not null, url text not null, api_key text not null,
  path_from text not null default '', path_to text not null default '', enabled bigint not null default 1);
create table if not exists seasons(
  inst_id bigint not null, series_id bigint not null, season bigint not null,
  title text not null, series_type text not null, monitored bigint not null,
  files bigint not null, aired bigint not null, pack bigint not null, single bigint not null,
  multi bigint not null, unknown bigint not null, groups text not null, qualities text not null,
  sources text not null, gb double precision not null, cur_rank bigint not null, path text not null,
  primary key(inst_id, series_id, season));
create table if not exists jobs(
  id {{PK}}, inst_id bigint not null, series_id bigint not null, season bigint not null,
  title text not null, old_file_ids text not null, release text not null, indexer text not null,
  size_gb double precision not null, status text not null, message text not null default '', progress bigint not null default 0,
  import_cmd bigint not null default 0, created bigint not null, updated bigint not null);
create table if not exists skips(key text primary key, until bigint not null, reason text not null default '');
create table if not exists events(id {{PK}}, ts bigint not null, level text not null, msg text not null);
create table if not exists kv(k text primary key, v text not null);
create table if not exists api_keys(id {{PK}}, name text not null, prefix text not null, hash text unique not null, created bigint not null, last_used bigint not null default 0);
`

// Open connects to databaseURL when set (postgres://...), otherwise SQLite under dir.
func Open(dir, databaseURL string) (*Store, error) {
	st := &Store{}
	pk := "integer primary key"
	if strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://") {
		db, err := sql.Open("pgx", databaseURL)
		if err != nil {
			return nil, err
		}
		st.DB, st.PG, pk = db, true, "bigserial primary key"
	} else {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "consolidarr.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1) // sqlite: one writer; keeps this simple and race-free
		st.DB = db
	}
	for _, stmt := range strings.Split(strings.ReplaceAll(schemaTmpl, "{{PK}}", pk), ";\n") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := st.DB.Exec(stmt); err != nil {
			return nil, fmt.Errorf("schema: %w", err)
		}
	}
	if err := st.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	_, _ = st.Exec(`delete from sessions where expires < ?`, time.Now().Unix()) // expired sessions are dead weight
	return st, nil
}

// A migration adds one column. Each is skipped when the column already exists, so a crash between the ALTER and
// the version bump (or two instances starting at once against Postgres) cannot make a later start fail with
// "duplicate column". Append only; never edit an entry.
type migration struct{ table, column, ddl string }

var migrations = []migration{
	{"jobs", "deleted", `alter table jobs add column deleted bigint not null default 0`},            // unix time the old files began to be deleted
	{"jobs", "download_id", `alter table jobs add column download_id text not null default ''`},     // the download client hash this job owns
	{"jobs", "expected", `alter table jobs add column expected bigint not null default 0`},          // how many episodes the pack must supply
	{"jobs", "expected_eps", `alter table jobs add column expected_eps text not null default '[]'`}, // exactly which episode numbers
	{"jobs", "import_at", `alter table jobs add column import_at bigint not null default 0`},        // when the current import attempt started
}

func (s *Store) hasColumn(table, column string) (bool, error) {
	if s.PG {
		var n int
		err := s.QueryRow(`select count(*) from information_schema.columns where table_schema = current_schema() and table_name = ? and column_name = ?`, table, column).Scan(&n)
		return n > 0, err
	}
	rows, err := s.DB.Query(`select name from pragma_table_info('` + table + `')`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) migrate() error {
	var raw sql.NullString
	if err := s.QueryRow(`select v from kv where k='schema_version'`).Scan(&raw); err != nil && err != sql.ErrNoRows {
		return err
	}
	v, _ := strconv.Atoi(raw.String)
	for i := v; i < len(migrations); i++ {
		m := migrations[i]
		has, err := s.hasColumn(m.table, m.column)
		if err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if !has {
			ddl := m.ddl
			if s.PG {
				ddl = strings.Replace(ddl, "add column", "add column if not exists", 1) // two instances starting at once
			}
			if _, err := s.DB.Exec(ddl); err != nil {
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		if _, err := s.Exec(`insert into kv(k,v) values('schema_version',?) on conflict(k) do update set v=excluded.v`, strconv.Itoa(i+1)); err != nil {
			return fmt.Errorf("migration %d: recording version: %w", i+1, err)
		}
	}
	if v < len(migrations) {
		// Jobs that were already importing when these columns appeared have no recorded delete time: treat the last
		// update as it, so they are judged by sensible ages instead of "deleted in 1970".
		_, _ = s.Exec(`update jobs set deleted = updated where status = 'importing' and deleted = 0`)
		_, _ = s.Exec(`update jobs set import_at = updated where status = 'importing' and import_at = 0`)
	}
	// One unfinished job per season, enforced by the database so two racing callers cannot both create one.
	// If old data already violates it the app still starts (relying on the application-level check in Grab/Adopt),
	// but say so loudly: the airtight guarantee is missing until the duplicate jobs are cleared.
	if _, err := s.DB.Exec(`create unique index if not exists jobs_one_active_v2 on jobs(inst_id, series_id, season) where status in ('reserving','downloading','ready','holding','importing')`); err != nil {
		log.Printf("WARNING: could not enforce one active job per season (%v); existing duplicate jobs must be cleared", err)
	} else {
		_, _ = s.DB.Exec(`drop index if exists jobs_one_active`) // superseded (it did not cover 'reserving'); only dropped once its replacement exists
	}
	return nil
}

type Settings struct {
	SchedulerEnabled bool     `json:"schedulerEnabled"`
	DryRun           bool     `json:"dryRun"`
	AutoDelete       bool     `json:"autoDelete"`
	AllowUnverified  bool     `json:"allowUnverified"` // delete even when ffprobe can't reach the files (default off)
	MaxInflight      int      `json:"maxInflight"`
	MaxPackGB        float64  `json:"maxPackGB"` // 0 = no size limit
	MinFiles         int      `json:"minFiles"`
	IncludeAnime     bool     `json:"includeAnime"`
	CycleMinutes     int      `json:"cycleMinutes"`
	ScanHours        int      `json:"scanHours"`
	LastResort       []string `json:"lastResortIndexers"`
}

func DefaultSettings() Settings {
	return Settings{DryRun: true, AutoDelete: true, MaxInflight: 4, MaxPackGB: 60, MinFiles: 3,
		CycleMinutes: 3, ScanHours: 6, LastResort: []string{"torrentleech"}}
}

func (s *Store) Settings() Settings {
	st := DefaultSettings()
	var v string
	if s.QueryRow(`select v from kv where k='settings'`).Scan(&v) == nil {
		_ = json.Unmarshal([]byte(v), &st)
	}
	return st
}

func (s *Store) SaveSettings(st Settings) error {
	b, _ := json.Marshal(st)
	_, err := s.Exec(`insert into kv(k,v) values('settings',?) on conflict(k) do update set v=excluded.v`, string(b))
	return err
}

func (s *Store) KV(k string) string {
	var v string
	_ = s.QueryRow(`select v from kv where k=?`, k).Scan(&v)
	return v
}

func (s *Store) SetKV(k, v string) {
	_, _ = s.Exec(`insert into kv(k,v) values(?,?) on conflict(k) do update set v=excluded.v`, k, v)
}

func (s *Store) Event(level, msg string) {
	_, _ = s.Exec(`insert into events(ts,level,msg) values(?,?,?)`, time.Now().Unix(), level, msg)
	_, _ = s.Exec(`delete from events where id < (select max(id) from events) - 5000`)
}

type Event struct {
	ID    int64  `json:"id"`
	TS    int64  `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

func (s *Store) Events(limit int) ([]Event, error) {
	rows, err := s.Query(`select id,ts,level,msg from events order by id desc limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Level, &e.Msg); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Skipped reports whether key is still inside a skip window.
func (s *Store) Skipped(key string) bool {
	var until int64
	err := s.QueryRow(`select until from skips where key=?`, key).Scan(&until)
	return err == nil && until > time.Now().Unix()
}

func (s *Store) Skip(key string, d time.Duration, reason string) {
	_, _ = s.Exec(`insert into skips(key,until,reason) values(?,?,?) on conflict(key) do update set until=excluded.until, reason=excluded.reason`,
		key, time.Now().Add(d).Unix(), reason)
}

var ErrNotFound = errors.New("not found")
