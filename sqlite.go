package sdk

// Box-side SQLite helper.
//
// Every apiself box that persists user state opens a SQLite database
// at {BoxDataDir}/db/<name>.db (resolved via sdk.BoxDBPath). The
// driver-specific pragma incantation needed to make that DB actually
// behave the way we want (WAL mode, sensible busy timeout, single
// writer concurrency) is fiddly and the apiself codebase has a
// history of getting it slightly wrong on individual boxes - 0.1.22
// through 0.1.26 of apiself-box-tts ran in journal_mode=DELETE
// because the legacy mattn/go-sqlite3 query string form was silently
// ignored by the modernc.org/sqlite driver every box uses.
//
// OpenBoxSQLite is the single source of truth for that pragma string
// so a new box can call it and forget the details. See
// memory/feedback_box_db_filename.md for the underlying bug history.

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // register the CGO-free driver
)

// SQLiteOptions controls the optional pragmas applied on top of the
// always-on defaults (WAL + busy_timeout(5000) + max 1 connection).
// Most boxes can pass `nil` and skip the struct entirely.
type SQLiteOptions struct {
	// ForeignKeys enables `PRAGMA foreign_keys=ON` so DELETE CASCADE
	// and other declared FK rules actually fire. Default off because
	// most box schemas don't declare FKs and the pragma is silently
	// ignored in that case.
	ForeignKeys bool

	// BusyTimeoutMs overrides the default 5000 ms busy timeout. Used
	// by boxes that legitimately expect long-running write contention
	// (rare). Zero = default.
	BusyTimeoutMs int
}

// OpenBoxSQLite opens the SQLite file at `path` with the box-standard
// pragma + connection setup:
//
//   - journal_mode = WAL          (concurrent reads next to a writer)
//   - busy_timeout = 5000 ms      (retry on lock contention)
//   - SetMaxOpenConns(1)          (SQLite is single-writer; let the
//                                  database/sql layer serialise so we
//                                  never trip SQLITE_BUSY in app code)
//
// Pass `nil` opts for the defaults. The returned *sql.DB is ready to
// use; the caller still owns Close().
//
// On-disk layout of a healthy WAL database after first write:
//
//	{name}.db        - main database (durable rows after checkpoint)
//	{name}.db-wal    - write-ahead log (pending writes)
//	{name}.db-shm    - shared memory map for cross-process coordination
//
// All three reappear automatically on next open if you delete the
// companions.
func OpenBoxSQLite(path string, opts *SQLiteOptions) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("OpenBoxSQLite: path required")
	}
	busyMs := 5000
	if opts != nil && opts.BusyTimeoutMs > 0 {
		busyMs = opts.BusyTimeoutMs
	}
	parts := []string{
		"_pragma=journal_mode(WAL)",
		fmt.Sprintf("_pragma=busy_timeout(%d)", busyMs),
	}
	if opts != nil && opts.ForeignKeys {
		parts = append(parts, "_pragma=foreign_keys(on)")
	}
	dsn := path + "?" + strings.Join(parts, "&")

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("OpenBoxSQLite: sql.Open: %w", err)
	}
	// SQLite has exactly one writer at a time. Letting database/sql
	// hand out N connections just means SQLITE_BUSY when the second
	// caller tries to write - serialise at the pool layer instead.
	db.SetMaxOpenConns(1)
	return db, nil
}
