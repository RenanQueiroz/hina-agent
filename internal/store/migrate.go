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

// migrationVersions returns the migration versions (e.g. "0001_init") in
// ascending order, derived from the embedded *.up.sql files.
func migrationVersions() ([]string, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var versions []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			versions = append(versions, strings.TrimSuffix(e.Name(), ".up.sql"))
		}
	}
	sort.Strings(versions)
	return versions, nil
}

// Migrate applies any pending forward (".up.sql") migrations and returns how
// many were applied. It is idempotent and safe to run on every startup.
func (s *Store) Migrate(ctx context.Context) (int, error) {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`,
	); err != nil {
		return 0, fmt.Errorf("ensure schema_migrations: %w", err)
	}

	versions, err := migrationVersions()
	if err != nil {
		return 0, err
	}

	applied := 0
	for _, version := range versions {
		var exists int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM schema_migrations WHERE version=?`, version,
		).Scan(&exists); err != nil {
			return applied, err
		}
		if exists > 0 {
			continue
		}

		raw, err := migrationsFS.ReadFile("migrations/" + version + ".up.sql")
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
				return applied, fmt.Errorf("apply migration %s: %w", version, err)
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

// MigrateDown rolls back the most recently applied migrations by running their
// ".down.sql", newest first, and returns how many were reverted. steps<=0
// reverts everything. Each rollback (the down DDL + removing the version row)
// runs in one transaction so a failure leaves the schema consistent.
func (s *Store) MigrateDown(ctx context.Context, steps int) (int, error) {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`,
	); err != nil {
		return 0, fmt.Errorf("ensure schema_migrations: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC`)
	if err != nil {
		return 0, err
	}
	var applied []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return 0, err
		}
		applied = append(applied, v)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	if steps <= 0 || steps > len(applied) {
		steps = len(applied)
	}

	reverted := 0
	for i := 0; i < steps; i++ {
		version := applied[i]
		raw, err := migrationsFS.ReadFile("migrations/" + version + ".down.sql")
		if err != nil {
			return reverted, fmt.Errorf("no down migration for %s: %w", version, err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return reverted, err
		}
		for _, stmt := range splitStatements(string(raw)) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return reverted, fmt.Errorf("revert migration %s: %w", version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=?`, version); err != nil {
			_ = tx.Rollback()
			return reverted, err
		}
		if err := tx.Commit(); err != nil {
			return reverted, err
		}
		reverted++
	}
	return reverted, nil
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
