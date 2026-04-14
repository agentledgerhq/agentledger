package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

func InitDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		return nil, fmt.Errorf("failed to set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		return nil, fmt.Errorf("failed to set busy timeout: %w", err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS budgets (
		agent_id TEXT PRIMARY KEY,
		limit_amount INTEGER NOT NULL,
		used_amount INTEGER NOT NULL DEFAULT 0,
		period TEXT NOT NULL DEFAULT 'cumulative',
		period_start DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS transactions (
		id TEXT PRIMARY KEY,
		agent_id TEXT NOT NULL,
		amount INTEGER NOT NULL,
		merchant TEXT NOT NULL,
		purpose TEXT NOT NULL,
		status TEXT NOT NULL,
		reason TEXT,
		card_id TEXT,
		metadata TEXT,
		expires_at DATETIME,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS vault (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		encrypted_payload TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS credits (
		id TEXT PRIMARY KEY,
		agent_id TEXT NOT NULL,
		amount INTEGER NOT NULL,
		reason TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	`
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	// Migration: add period columns to existing budgets tables that lack them.
	// SQLite doesn't support IF NOT EXISTS on ALTER TABLE, so we check first.
	if err := migrateAddColumn(db, "budgets", "period", "TEXT NOT NULL DEFAULT 'cumulative'"); err != nil {
		return nil, fmt.Errorf("failed to migrate budgets.period: %w", err)
	}
	if err := migrateAddColumn(db, "budgets", "period_start", "DATETIME DEFAULT CURRENT_TIMESTAMP"); err != nil {
		return nil, fmt.Errorf("failed to migrate budgets.period_start: %w", err)
	}
	if err := migrateAddColumn(db, "transactions", "card_id", "TEXT"); err != nil {
		return nil, fmt.Errorf("failed to migrate transactions.card_id: %w", err)
	}
	if err := migrateAddColumn(db, "transactions", "reason", "TEXT"); err != nil {
		return nil, fmt.Errorf("failed to migrate transactions.reason: %w", err)
	}
	if err := migrateAddColumn(db, "transactions", "metadata", "TEXT"); err != nil {
		return nil, fmt.Errorf("failed to migrate transactions.metadata: %w", err)
	}
	if err := migrateAddColumn(db, "transactions", "expires_at", "DATETIME"); err != nil {
		return nil, fmt.Errorf("failed to migrate transactions.expires_at: %w", err)
	}

	return db, nil
}

// validIdentifier checks that a SQL identifier contains only safe characters
// (alphanumeric and underscores). This prevents SQL injection through
// table/column names, which cannot use parameterized queries in SQLite.
func validIdentifier(s string) bool {
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return len(s) > 0
}

// migrateAddColumn adds a column to a table if it doesn't already exist.
func migrateAddColumn(db *sql.DB, table, column, colDef string) error {
	if !validIdentifier(table) || !validIdentifier(column) {
		return fmt.Errorf("invalid identifier: table=%q column=%q", table, column)
	}

	// Check if column exists using pragma
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil // Column already exists
		}
	}

	// Column doesn't exist, add it
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, colDef))
	return err
}
