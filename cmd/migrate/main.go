package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dsn := os.Getenv("DB_MIGRATION_DSN")
	if dsn == "" {
		log.Fatal("DB_MIGRATION_DSN is required for migrations")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		log.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
		log.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
		applied_at DATETIME(6) NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin`); err != nil {
		log.Fatal(err)
	}
	migrationDir := env("MIGRATIONS_DIR", "/app/migrations")
	files, err := filepath.Glob(filepath.Join(migrationDir, "*.sql"))
	if err != nil {
		log.Fatal(err)
	}
	sort.Strings(files)
	for _, file := range files {
		version := strings.TrimSuffix(filepath.Base(file), ".sql")
		var exists int
		err := db.QueryRowContext(ctx, "SELECT 1 FROM schema_migrations WHERE version = ?", version).Scan(&exists)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			log.Fatal(err)
		}
		content, err := os.ReadFile(file)
		if err != nil {
			log.Fatal(err)
		}
		for _, statement := range strings.Split(string(content), ";") {
			statement = strings.TrimSpace(statement)
			if statement == "" {
				continue
			}
			if _, err := db.ExecContext(ctx, statement); err != nil {
				log.Fatalf("migration %s failed: %v", version, err)
			}
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (?, UTC_TIMESTAMP(6))", version); err != nil {
			log.Fatalf("record migration %s: %v", version, err)
		}
		fmt.Println("migration applied:", version)
	}
	destinationID := env("DESTINATION_ID", "mock-target")
	destinationURL := env("DESTINATION_URL", "http://mock-target:8090/notify")
	if _, err := db.ExecContext(ctx, `INSERT INTO destination (destination_id, target_url, fast_fail_enabled, updated_at)
		VALUES (?, ?, FALSE, UTC_TIMESTAMP(6)) ON DUPLICATE KEY UPDATE target_url = VALUES(target_url)`, destinationID, destinationURL); err != nil {
		log.Fatal(err)
	}
	fmt.Println("destination seeded:", destinationID)
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
