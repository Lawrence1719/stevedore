package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps a SQLite database connection.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path and runs any pending migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("store: open db: %w", err)
	}
	// SQLite performs best with a single writer.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB exposes the raw *sql.DB for use in tests or ad-hoc queries.
func (s *Store) DB() *sql.DB {
	return s.db
}

// migrate applies any SQL migration files not yet recorded in schema_version.
func (s *Store) migrate() error {
	// Ensure schema_version exists so we can read from it safely.
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}

	// Sort by filename so migrations run in order.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		// Extract the version number from the filename prefix (e.g. "0001_init.sql" → 1).
		var version int
		if _, err := fmt.Sscanf(entry.Name(), "%04d_", &version); err != nil {
			return fmt.Errorf("bad migration filename %q: %w", entry.Name(), err)
		}

		var exists int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version = ?`, version).Scan(&exists)
		if exists > 0 {
			continue // already applied
		}

		data, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}

		if _, err := s.db.Exec(string(data)); err != nil {
			return fmt.Errorf("migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}
