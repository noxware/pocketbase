package migrate

import (
	"fmt"
	"os"
	"path"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/fatih/color"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/tools/inflector"
	"github.com/spf13/cast"
)

const migrationsTable = "_migrations"

// Runner defines a simple struct for managing the execution of db migrations.
type Runner struct {
	db             *dbx.DB
	migrationsList MigrationsList
	tableName      string
}

// NewRunner creates and initializes a new db migrations Runner instance.
func NewRunner(db *dbx.DB, migrationsList MigrationsList) (*Runner, error) {
	runner := &Runner{
		db:             db,
		migrationsList: migrationsList,
		tableName:      migrationsTable,
	}

	if err := runner.createMigrationsTable(); err != nil {
		return nil, err
	}

	return runner, nil
}

// Run interactively executes the current runner with the provided args.
//
// The following commands are supported:
// - up                        - applies all migrations
// - down [n]                  - reverts the last n applied migrations
// - create NEW_MIGRATION_NAME - create NEW_MIGRATION_NAME.go file from a migration template
func (r *Runner) Run(args ...string) error {
	cmd := "up"
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "up":
		applied, err := r.Up()
		if err != nil {
			color.Red(err.Error())
			return err
		}

		if len(applied) == 0 {
			color.Green("No new migrations to apply.")
		} else {
			for _, file := range applied {
				color.Green("Applied %s", file)
			}
		}

		return nil
	case "down":
		toRevertCount := 1
		if len(args) > 1 {
			toRevertCount = cast.ToInt(args[1])
			if toRevertCount < 0 {
				// revert all applied migrations
				toRevertCount = len(r.migrationsList.Items())
			}
		}

		confirm := false
		prompt := &survey.Confirm{
			Message: fmt.Sprintf("Do you really want to revert the last %d applied migration(s)?", toRevertCount),
		}
		survey.AskOne(prompt, &confirm)
		if !confirm {
			fmt.Println("The command has been cancelled")
			return nil
		}

		reverted, err := r.Down(toRevertCount)
		if err != nil {
			color.Red(err.Error())
			return err
		}

		if len(reverted) == 0 {
			color.Green("No migrations to revert.")
		} else {
			for _, file := range reverted {
				color.Green("Reverted %s", file)
			}
		}

		return nil
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("Missing migration file name")
		}

		name := args[1]

		var dir string
		if len(args) == 3 {
			dir = args[2]
		}
		if dir == "" {
			// If not specified, auto point to the default migrations folder.
			//
			// NB!
			// Since the create command makes sense only during development,
			// it is expected the user to be in the app working directory
			// and to be using `go run ...`
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			dir = path.Join(wd, "migrations")
		}

		resultFilePath := path.Join(
			dir,
			fmt.Sprintf("%d_%s.go", time.Now().Unix(), inflector.Snakecase(name)),
		)

		confirm := false
		prompt := &survey.Confirm{
			Message: fmt.Sprintf("Do you really want to create migration %q?", resultFilePath),
		}
		survey.AskOne(prompt, &confirm)
		if !confirm {
			fmt.Println("The command has been cancelled")
			return nil
		}

		// ensure that migrations dir exist
		if err := os.MkdirAll(dir, os.ModePerm); err != nil {
			return err
		}

		if err := os.WriteFile(resultFilePath, []byte(createTemplateContent), 0644); err != nil {
			return fmt.Errorf("Failed to save migration file %q\n", resultFilePath)
		}

		fmt.Printf("Successfully created file %q\n", resultFilePath)
		return nil
	default:
		return fmt.Errorf("Unsupported command: %q\n", cmd)
	}
}

// Up executes all unapplied migrations for the provided runner.
//
// On success returns list with the applied migrations file names.
func (r *Runner) Up() ([]string, error) {
	applied := []string{}

	err := r.db.Transactional(func(tx *dbx.Tx) error {
		for _, m := range r.migrationsList.Items() {
			// skip applied
			if r.isMigrationApplied(tx, m.file) {
				continue
			}

			if err := m.up(tx); err != nil {
				return fmt.Errorf("Failed to apply migration %s: %w", m.file, err)
			}

			if err := r.saveAppliedMigration(tx, m.file); err != nil {
				return fmt.Errorf("Failed to save applied migration info for %s: %w", m.file, err)
			}

			applied = append(applied, m.file)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}
	return applied, nil
}

// Down reverts the last `toRevertCount` applied migrations.
//
// On success returns list with the reverted migrations file names.
func (r *Runner) Down(toRevertCount int) ([]string, error) {
	applied := []string{}

	err := r.db.Transactional(func(tx *dbx.Tx) error {
		totalReverted := 0

		for i := len(r.migrationsList.Items()) - 1; i >= 0; i-- {
			m := r.migrationsList.Item(i)

			// skip unapplied
			if !r.isMigrationApplied(tx, m.file) {
				continue
			}

			// revert limit reached
			if toRevertCount-totalReverted <= 0 {
				break
			}

			if err := m.down(tx); err != nil {
				return fmt.Errorf("Failed to revert migration %s: %w", m.file, err)
			}

			if err := r.saveRevertedMigration(tx, m.file); err != nil {
				return fmt.Errorf("Failed to save reverted migration info for %s: %w", m.file, err)
			}

			applied = append(applied, m.file)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}
	return applied, nil
}

func (r *Runner) createMigrationsTable() error {
	rawQuery := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %v (file VARCHAR(255) PRIMARY KEY NOT NULL, applied INTEGER NOT NULL)",
		r.db.QuoteTableName(r.tableName),
	)

	_, err := r.db.NewQuery(rawQuery).Execute()

	return err
}

func (r *Runner) isMigrationApplied(tx dbx.Builder, file string) bool {
	var exists bool

	err := tx.Select("count(*)").
		From(r.tableName).
		Where(dbx.HashExp{"file": file}).
		Limit(1).
		Row(&exists)

	return err == nil && exists
}

func (r *Runner) saveAppliedMigration(tx dbx.Builder, file string) error {
	_, err := tx.Insert(r.tableName, dbx.Params{
		"file":    file,
		"applied": time.Now().Unix(),
	}).Execute()

	return err
}

func (r *Runner) saveRevertedMigration(tx dbx.Builder, file string) error {
	_, err := tx.Delete(r.tableName, dbx.HashExp{"file": file}).Execute()

	return err
}
