package database

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrations embed.FS

func Migrate(databaseURL string) error {
	source, err := iofs.New(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("open migrations: %w", err)
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open migration database: %w", err)
	}

	driver, err := migratepostgres.WithInstance(db, &migratepostgres.Config{})
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("create migration driver: %w", err)
	}

	migrator, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("create migrator: %w", err)
	}

	if err := migrator.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		_, _ = migrator.Close()
		return fmt.Errorf("apply migrations: %w", err)
	}

	sourceErr, databaseErr := migrator.Close()
	if sourceErr != nil {
		return fmt.Errorf("close migration source: %w", sourceErr)
	}
	if databaseErr != nil {
		return fmt.Errorf("close migration database: %w", databaseErr)
	}

	return nil
}
