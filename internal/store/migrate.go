package store

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies any pending forward-only migrations and returns how many were
// applied. It is idempotent and safe to run on every startup.
func (s *Store) Migrate(ctx context.Context) (int, error) {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`,
	); err != nil {
		return 0, fmt.Errorf("ensure schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return 0, fmt.Errorf("read migrations: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	applied := 0
	for _, f := range files {
		version := strings.TrimSuffix(f, ".sql")

		var exists int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM schema_migrations WHERE version=?`, version,
		).Scan(&exists); err != nil {
			return applied, err
		}
		if exists > 0 {
			continue
		}

		raw, err := migrationsFS.ReadFile("migrations/" + f)
		if err != nil {
			return applied, err
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return applied, err
		}
		for _, stmt := range splitStatements(string(raw)) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return applied, fmt.Errorf("apply migration %s: %w", f, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`,
			version, time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			_ = tx.Rollback()
			return applied, err
		}
		if err := tx.Commit(); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

// splitStatements splits a migration file into individual statements on ";".
// It first strips "--" line comments so a semicolon inside a comment does not
// split a statement. The v0 schema is plain DDL with no "--" or ";" inside
// string literals, so this is safe.
func splitStatements(sqlText string) []string {
	var stripped strings.Builder
	for _, line := range strings.Split(sqlText, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		stripped.WriteString(line)
		stripped.WriteByte('\n')
	}
	var out []string
	for _, part := range strings.Split(stripped.String(), ";") {
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return out
}
