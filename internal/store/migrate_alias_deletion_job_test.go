package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAliasDeletionJobSQLiteV8ConvergenceIsAdditiveAndReentrant(t *testing.T) {
	t.Parallel()
	for _, withGroups := range []bool{false, true} {
		for _, withJobs := range []bool{false, true} {
			t.Run(fmt.Sprintf("groups=%t/jobs=%t", withGroups, withJobs), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				dsn, err := sqliteDSN(filepath.Join(t.TempDir(), "v8.db"))
				if err != nil {
					t.Fatal(err)
				}
				raw, err := sql.Open("sqlite", dsn)
				if err != nil {
					t.Fatal(err)
				}
				raw.SetMaxOpenConns(1)
				t.Cleanup(func() { _ = raw.Close() })
				s := New(raw)
				statements := schemaV8
				if withGroups {
					statements = schemaV8WithMailGroups
				}
				for _, statement := range statements {
					if _, err := raw.ExecContext(ctx, statement); err != nil {
						t.Fatalf("create prior v8 schema: %v", err)
					}
				}
				if _, err := raw.ExecContext(ctx, `PRAGMA user_version = 8`); err != nil {
					t.Fatal(err)
				}
				owner := createAliasDeletionJobTestAdmin(t, s, "preserved-owner")
				job := aliasDeletionJobTestFixture("preserved-job", owner.ID)
				job.Items[0].Done, job.Items[0].Deleted = true, true
				if withGroups {
					if _, err := raw.ExecContext(ctx, `INSERT INTO mail_groups(id, name, name_key, created_at, updated_at)
						VALUES(1, 'Preserved group', 'preserved group', 10, 20)`); err != nil {
						t.Fatal(err)
					}
				}
				if withJobs {
					// Simulate the table already existing while its indexes have
					// not yet been converged; progress must not be recreated.
					if _, err := raw.ExecContext(ctx, createAliasDeletionJobsTable); err != nil {
						t.Fatal(err)
					}
					if _, err := raw.ExecContext(ctx, `CREATE UNIQUE INDEX alias_deletion_jobs_active_admin_idx ON alias_deletion_jobs(admin_id) WHERE status IN ('queued', 'running')`); err != nil {
						t.Fatal(err)
					}
					mustCreateAliasDeletionJob(t, s, job)
				}
				for pass := range 3 {
					if err := s.Migrate(ctx); err != nil {
						t.Fatalf("convergence pass %d: %v", pass, err)
					}
					var version int
					if err := raw.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
						t.Fatal(err)
					}
					if version != 8 {
						t.Fatalf("schema version = %d, want 8", version)
					}
					if !withJobs && pass == 0 {
						mustCreateAliasDeletionJob(t, s, job)
					}
					assertAliasDeletionJobEqual(t, mustGetAliasDeletionJob(t, s, job), job)
					assertAliasDeletionJobSQLiteSchema(t, raw)
					if withGroups {
						var name, key string
						var createdAt, updatedAt int64
						if err := raw.QueryRowContext(ctx,
							`SELECT name, name_key, created_at, updated_at FROM mail_groups WHERE id = 1`,
						).Scan(&name, &key, &createdAt, &updatedAt); err != nil {
							t.Fatal(err)
						}
						if name != "Preserved group" || key != "preserved group" || createdAt != 10 || updatedAt != 20 {
							t.Fatalf("parallel mail-group schema data changed: %q %q %d %d", name, key, createdAt, updatedAt)
						}
					}
				}
				if _, err := raw.ExecContext(ctx, `DELETE FROM admins WHERE id = ?`, owner.ID); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM alias_deletion_jobs`).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatal("job owner foreign key did not cascade deleted administrator")
				}
			})
		}
	}
}

func TestAliasDeletionJobSQLiteFreshSchema(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionJobTestStore(t, ":memory:")
	assertAliasDeletionJobSQLiteSchema(t, s.db)
}

func TestAliasDeletionJobPostgresConvergenceAllSupportedVersions(t *testing.T) {
	t.Parallel()
	if schemaVersion != 8 {
		t.Fatalf("durable schema version = %d, want 8", schemaVersion)
	}
	for _, version := range []int{0, 3, 4, 5, 6, 7, 8} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			t.Parallel()
			// Reuse the repository's PostgreSQL migration capture driver. This
			// validates actual migration routing without a deployed database.
			capture := &postgresMigrationCaptureDriver{version: version}
			driverName := fmt.Sprintf("alias-deletion-postgres-migrate-%p", capture)
			sql.Register(driverName, capture)
			raw, err := sql.Open(driverName, "")
			if err != nil {
				t.Fatal(err)
			}
			raw.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = raw.Close() })
			s := newStore(raw, dialectPostgres)
			for pass := range 2 {
				capture.statements = nil
				if err := s.Migrate(context.Background()); err != nil {
					t.Fatalf("PostgreSQL convergence pass %d: %v", pass, err)
				}
				for _, statement := range []string{
					createAliasDeletionJobsTable, createAliasDeletionJobsLatestIndex, createAliasDeletionJobsActiveIndex,
				} {
					if !containsNormalizedSQL(capture.statements, statement) {
						t.Errorf("missing PostgreSQL job schema convergence: %s", statement)
					}
				}
				if !containsNormalizedSQL(capture.statements, postgresMailGroupSchema[0]) ||
					!containsNormalizedSQL(capture.statements, postgresMigrateV7ToV8[0]) {
					t.Fatal("job convergence displaced existing mail-group or mailbox-setting convergence")
				}
				for _, statement := range capture.statements {
					normalized := normalizeSQL(statement)
					if strings.Contains(normalized, "alias_deletion_jobs") &&
						!strings.HasPrefix(normalized, "create table if not exists ") &&
						!strings.HasPrefix(normalized, "create index if not exists ") &&
						!strings.HasPrefix(normalized, "create unique index if not exists ") {
						if normalized != normalizeSQL(createAliasDeletionJobsActiveIndex) {
							t.Errorf("unexpected destructive job convergence: %s", normalized)
						}
					}
					if (pass == 1 || version == 8) && strings.HasPrefix(normalized, "update schema_migrations set version") {
						t.Errorf("v8 convergence unnecessarily rewrote schema version: %s", normalized)
					}
				}
				capture.version = 8
			}
		})
	}
}

func TestAliasDeletionJobSchemaUsesPortableBoundedProgressColumns(t *testing.T) {
	t.Parallel()
	for _, statements := range [][]string{sqliteSchemaConvergence, postgresSchemaConvergence} {
		if !containsNormalizedSQL(statements, createAliasDeletionJobsTable) ||
			!containsNormalizedSQL(statements, createAliasDeletionJobsLatestIndex) ||
			!containsNormalizedSQL(statements, createAliasDeletionJobsActiveIndex) {
			t.Fatal("database dialect omitted portable job schema")
		}
	}
	table := normalizeSQL(createAliasDeletionJobsTable)
	for _, wanted := range []string{
		"id text not null", "primary key(admin_id, id)", "admin_id bigint not null references admins(id) on delete cascade",
		"items_json text not null", "created_at bigint not null", "updated_at bigint not null",
		"check(status in ('queued', 'running', 'completed', 'interrupted'))",
	} {
		if !strings.Contains(table, wanted) {
			t.Errorf("portable job schema missing %q", wanted)
		}
	}
}

func assertAliasDeletionJobSQLiteSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(alias_deletion_jobs)`)
	if err != nil {
		t.Fatal(err)
	}
	columns := make(map[string]string)
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		columns[name] = strings.ToUpper(columnType)
		if notNull != 1 {
			t.Errorf("job column %q permits null", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Error(err)
	}
	_ = rows.Close()
	want := map[string]string{
		"id": "TEXT", "admin_id": "BIGINT", "request_id": "TEXT", "status": "TEXT",
		"items_json": "TEXT", "created_at": "BIGINT", "updated_at": "BIGINT",
	}
	if !reflect.DeepEqual(columns, want) {
		t.Fatalf("progress schema columns = %v, want %v (no credential fields)", columns, want)
	}
	for _, index := range []struct {
		name string
		want string
	}{
		{"alias_deletion_jobs_admin_created_idx", "on alias_deletion_jobs(admin_id, created_at desc, id desc)"},
	} {
		var statement string
		if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, index.name).Scan(&statement); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(normalizeSQL(statement), index.want) {
			t.Errorf("index %s = %s", index.name, statement)
		}
	}
	var activeIndexCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'alias_deletion_jobs_active_admin_idx'`).Scan(&activeIndexCount); err != nil {
		t.Fatal(err)
	}
	if activeIndexCount != 0 {
		t.Fatal("obsolete single-active-job index remains")
	}
}
