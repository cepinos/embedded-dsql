package embeddeddsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

var shared *DB

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := Start(ctx, Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "start embedded-dsql: %v\n", err)
		return 1
	}
	shared = db
	code := m.Run()
	if err := db.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop embedded-dsql: %v\n", err)
		return 1
	}
	return code
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	ctx := testCtx(t)
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close(context.Background()))
	})
	return conn
}

func requireSQLState(t *testing.T, err error, code string) *pgconn.PgError {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "want a PostgreSQL error %s, got %v", code, err)
	require.Equal(t, code, pgErr.Code, pgErr.Message)
	return pgErr
}

func TestPgxHappyPath(t *testing.T) {
	ctx := testCtx(t)
	conn := connect(t, shared.DSN())

	_, err := conn.Exec(ctx, `CREATE TABLE happy (id uuid PRIMARY KEY, name text NOT NULL, n bigint, doc jsonb)`)
	require.NoError(t, err)

	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err = tx.Exec(ctx, `INSERT INTO happy (id, name, n, doc) VALUES (gen_random_uuid(), $1, $2, $3)`,
			fmt.Sprintf("row-%d", i), int64(i), map[string]any{"i": i})
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit(ctx))

	var count int
	var total int64
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*), sum(n)::bigint FROM happy WHERE name LIKE $1`, "row-%").Scan(&count, &total))
	require.Equal(t, 3, count)
	require.Equal(t, int64(3), total)

	tag, err := conn.Exec(ctx, `UPDATE happy SET n = n + 10 WHERE n >= $1`, int64(1))
	require.NoError(t, err)
	require.Equal(t, int64(2), tag.RowsAffected())

	var name string
	require.NoError(t, conn.QueryRow(ctx, `SELECT name FROM happy WHERE n = $1 FOR UPDATE`, int64(12)).Scan(&name))
	require.Equal(t, "row-2", name)

	tag, err = conn.Exec(ctx, `DELETE FROM happy WHERE n > $1`, int64(10))
	require.NoError(t, err)
	require.Equal(t, int64(2), tag.RowsAffected())
}

func TestDatabaseSQLHappyPath(t *testing.T) {
	ctx := testCtx(t)
	db, err := sql.Open("pgx", shared.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	_, err = db.ExecContext(ctx, `CREATE TABLE sqlpath (id bigint PRIMARY KEY, label varchar(20))`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO sqlpath VALUES ($1, $2), ($3, $4)`, 1, "one", 2, "two")
	require.NoError(t, err)

	rows, err := db.QueryContext(ctx, `SELECT label FROM sqlpath ORDER BY id`)
	require.NoError(t, err)
	var labels []string
	for rows.Next() {
		var l string
		require.NoError(t, rows.Scan(&l))
		labels = append(labels, l)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []string{"one", "two"}, labels)

	_, err = db.ExecContext(ctx, `CREATE TABLE sqlpath_bad (id serial)`)
	requireSQLState(t, err, "0A000")
}

func TestRejectedStatements(t *testing.T) {
	ctx := testCtx(t)
	conn := connect(t, shared.DSN())
	_, err := conn.Exec(ctx, `CREATE TABLE rejected (id bigint PRIMARY KEY, v int)`)
	require.NoError(t, err)

	cases := []struct {
		name string
		sql  string
		args []any
		code string
	}{
		{name: "serial column over simple protocol", sql: `CREATE TABLE rj_serial (id serial)`, code: "0A000"},
		{name: "extension", sql: `CREATE EXTENSION pgcrypto`, code: "0A000"},
		{name: "truncate", sql: `TRUNCATE rejected`, code: "0A000"},
		{name: "synchronous index", sql: `CREATE INDEX rj_idx ON rejected (v)`, code: "0A000"},
		{name: "share lock over extended protocol", sql: `SELECT v FROM rejected WHERE id = $1 FOR SHARE`, args: []any{int64(1)}, code: "0A000"},
		{name: "volatile index expression", sql: `CREATE INDEX ASYNC rj_now ON rejected (now())`, code: "42P17"},
		{name: "too many VALUES rows", sql: valuesInsertSQL(3001), code: "25006"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := conn.Exec(ctx, tc.sql, tc.args...)
			requireSQLState(t, err, tc.code)
			// The connection stays usable after a rejection.
			var one int
			require.NoError(t, conn.QueryRow(ctx, `SELECT 1`).Scan(&one))
		})
	}
}

func valuesInsertSQL(n int) string {
	sql := "INSERT INTO rejected (id, v) VALUES "
	for i := 0; i < n; i++ {
		if i > 0 {
			sql += ","
		}
		sql += fmt.Sprintf("(%d, 0)", i+100000)
	}
	return sql
}

func TestTransactionDiscipline(t *testing.T) {
	ctx := testCtx(t)
	conn := connect(t, shared.DSN())
	_, err := conn.Exec(ctx, `CREATE TABLE disc (id bigint PRIMARY KEY)`)
	require.NoError(t, err)

	t.Run("DDL then DML in one transaction", func(t *testing.T) {
		tx, err := conn.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `CREATE TABLE disc_a (id bigint)`)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `INSERT INTO disc VALUES ($1)`, int64(1))
		requireSQLState(t, err, "25006")
		require.NoError(t, tx.Rollback(ctx))
	})

	t.Run("a rejection aborts the block and COMMIT rolls back", func(t *testing.T) {
		tx, err := conn.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `INSERT INTO disc VALUES ($1)`, int64(7))
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `TRUNCATE disc`)
		requireSQLState(t, err, "0A000")
		_, err = tx.Exec(ctx, `INSERT INTO disc VALUES ($1)`, int64(8))
		requireSQLState(t, err, "25P02")
		require.ErrorIs(t, tx.Commit(ctx), pgx.ErrTxCommitRollback)

		var n int
		require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM disc`).Scan(&n))
		require.Equal(t, 0, n)
	})

	t.Run("a cached statement is re-checked in a later transaction", func(t *testing.T) {
		_, err := conn.Exec(ctx, `INSERT INTO disc VALUES ($1)`, int64(20))
		require.NoError(t, err)
		tx, err := conn.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `CREATE TABLE disc_b (id bigint)`)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `INSERT INTO disc VALUES ($1)`, int64(21))
		requireSQLState(t, err, "25006")
		require.NoError(t, tx.Rollback(ctx))
	})
}

func TestAsyncIndexAndSysJobs(t *testing.T) {
	ctx := testCtx(t)
	conn := connect(t, shared.DSN())
	_, err := conn.Exec(ctx, `CREATE TABLE ai (id bigint PRIMARY KEY, email text)`)
	require.NoError(t, err)

	var jobID string
	require.NoError(t, conn.QueryRow(ctx, `CREATE INDEX ASYNC ai_email ON ai (email)`).Scan(&jobID))
	require.Regexp(t, `^[a-z0-9]{26}$`, jobID)

	var status, objectName string
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT status, object_name FROM sys.jobs WHERE job_id = $1`, jobID).Scan(&status, &objectName))
	require.Equal(t, "completed", status)
	require.Equal(t, "public.ai_email", objectName)

	var done bool
	require.NoError(t, conn.QueryRow(ctx, `SELECT sys.wait_for_job($1)`, jobID).Scan(&done))
	require.True(t, done)
	require.NoError(t, conn.QueryRow(ctx, fmt.Sprintf(`SELECT sys.wait_for_job('%s')`, jobID)).Scan(&done))
	require.True(t, done)

	// The index really exists on the backend.
	var indexes int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE tablename = 'ai' AND indexname = 'ai_email'`).Scan(&indexes))
	require.Equal(t, 1, indexes)

	// Simple protocol returns the job id too.
	var jobID2 string
	require.NoError(t, conn.QueryRow(ctx, `CREATE UNIQUE INDEX ASYNC ai_id_email ON ai (id, email)`,
		pgx.QueryExecModeSimpleProtocol).Scan(&jobID2))
	require.NotEqual(t, jobID, jobID2)

	// A duplicate key fails the job, not its submission.
	_, err = conn.Exec(ctx, `INSERT INTO ai VALUES (1, 'a'), (2, 'a')`)
	require.NoError(t, err)
	var failedJob string
	require.NoError(t, conn.QueryRow(ctx, `CREATE UNIQUE INDEX ASYNC ai_unique_email ON ai (email)`).Scan(&failedJob))
	require.NoError(t, conn.QueryRow(ctx, `SELECT status FROM sys.jobs WHERE job_id = $1`, failedJob).Scan(&status))
	require.Equal(t, "failed", status)
	require.NoError(t, conn.QueryRow(ctx, `SELECT sys.wait_for_job($1)`, failedJob).Scan(&done))
	require.False(t, done)

	// Unknown sys.jobs columns are refused like a real relation would.
	_, err = conn.Exec(ctx, `SELECT nope FROM sys.jobs`)
	requireSQLState(t, err, "42703")
}

func TestRepeatableReadWriteConflict(t *testing.T) {
	ctx := testCtx(t)
	a := connect(t, shared.DSN())
	b := connect(t, shared.DSN())
	_, err := a.Exec(ctx, `CREATE TABLE rr (id bigint PRIMARY KEY, v int)`)
	require.NoError(t, err)
	_, err = a.Exec(ctx, `INSERT INTO rr VALUES (1, 0)`)
	require.NoError(t, err)

	txA, err := a.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	var v int
	require.NoError(t, txA.QueryRow(ctx, `SELECT v FROM rr WHERE id = $1`, int64(1)).Scan(&v))

	txB, err := b.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	_, err = txB.Exec(ctx, `UPDATE rr SET v = v + 1 WHERE id = $1`, int64(1))
	require.NoError(t, err)
	require.NoError(t, txB.Commit(ctx))

	_, err = txA.Exec(ctx, `UPDATE rr SET v = v + 1 WHERE id = $1`, int64(1))
	requireSQLState(t, err, "40001")
	require.NoError(t, txA.Rollback(ctx))
}

func TestStaleCatalogOC001(t *testing.T) {
	ctx := testCtx(t)
	a := connect(t, shared.DSN())
	b := connect(t, shared.DSN())
	_, err := a.Exec(ctx, `CREATE TABLE oc (id bigint PRIMARY KEY)`)
	require.NoError(t, err)

	tx, err := a.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT count(*) FROM oc`)
	require.NoError(t, err)

	_, err = b.Exec(ctx, `CREATE TABLE oc_other (id bigint)`)
	require.NoError(t, err)

	_, err = tx.Exec(ctx, `SELECT count(*) FROM oc`)
	pgErr := requireSQLState(t, err, "40001")
	require.Contains(t, pgErr.Message, "OC001")
	require.NoError(t, tx.Rollback(ctx))

	// The retry sees the fresh catalog.
	tx, err = a.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT count(*) FROM oc`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
}

func TestDropPrimaryKeyColumn(t *testing.T) {
	ctx := testCtx(t)
	conn := connect(t, shared.DSN())
	_, err := conn.Exec(ctx, `CREATE TABLE dpk (id bigint PRIMARY KEY, x int, y int)`)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `ALTER TABLE dpk DROP COLUMN x`)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `ALTER TABLE dpk DROP COLUMN y, DROP COLUMN id`)
	pgErr := requireSQLState(t, err, "0A000")
	require.Equal(t, "cannot drop primary key column id", pgErr.Message)
}

func TestPostgresDSNBypassesValidation(t *testing.T) {
	ctx := testCtx(t)
	conn := connect(t, shared.DSN())
	_, err := conn.Exec(ctx, `CREATE TABLE bypass (id bigint PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `TRUNCATE bypass`)
	requireSQLState(t, err, "0A000")

	direct := connect(t, shared.PostgresDSN())
	_, err = direct.Exec(ctx, `TRUNCATE bypass`)
	require.NoError(t, err)
}

func TestPassthrough(t *testing.T) {
	ctx := testCtx(t)
	db, err := Start(ctx, Options{Passthrough: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Stop()) })
	require.Equal(t, db.PostgresDSN(), db.DSN())

	conn := connect(t, db.DSN())
	_, err = conn.Exec(ctx, `CREATE TABLE pt (id serial PRIMARY KEY)`)
	require.NoError(t, err)
}

func TestStopLeavesNoProcess(t *testing.T) {
	ctx := testCtx(t)
	db, err := Start(ctx, Options{})
	require.NoError(t, err)

	conn, err := pgx.Connect(ctx, db.DSN())
	require.NoError(t, err)
	pid := db.postmasterP
	require.NotZero(t, pid)
	require.True(t, processAlive(pid))
	root := db.layout.root

	require.NoError(t, db.Stop())
	require.NoError(t, db.Stop(), "Stop is idempotent")
	require.False(t, processAlive(pid), "postmaster %d still running", pid)
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)

	// The client connection was closed by Stop.
	require.Error(t, conn.Ping(ctx))
	require.NoError(t, conn.Close(ctx))
}

func TestStartStopsALeftoverInstanceOfTheSameDir(t *testing.T) {
	ctx := testCtx(t)
	dir := searchableTempDir(t)

	first, err := Start(ctx, Options{RuntimeDir: dir})
	require.NoError(t, err)
	leftover := first.postmasterP
	require.True(t, processAlive(leftover))
	// Simulate a test binary that died without calling Stop.
	require.NoError(t, first.proxy.Close())

	second, err := Start(ctx, Options{RuntimeDir: dir})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Stop()) })
	require.NoError(t, waitGone(leftover, 30*time.Second))

	conn := connect(t, second.DSN())
	var one int
	require.NoError(t, conn.QueryRow(ctx, `SELECT 1`).Scan(&one))
}

// searchableTempDir returns a runtime dir PostgreSQL can reach even when the
// tests run as root and PostgreSQL runs as nobody.
func searchableTempDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	tmp := filepath.Clean(os.TempDir())
	for d := base; d != tmp && d != filepath.Dir(d); d = filepath.Dir(d) {
		require.NoError(t, os.Chmod(d, 0o755))
	}
	return filepath.Join(base, "rt")
}

func TestParallelInstances(t *testing.T) {
	ctx := testCtx(t)
	dbs := make([]*DB, 3)
	errs := make(chan error, len(dbs))
	for i := range dbs {
		go func() {
			db, err := Start(ctx, Options{})
			dbs[i] = db
			errs <- err
		}()
	}
	for range dbs {
		require.NoError(t, <-errs)
	}
	for _, db := range dbs {
		t.Cleanup(func() { require.NoError(t, db.Stop()) })
		conn := connect(t, db.DSN())
		_, err := conn.Exec(ctx, `CREATE TABLE only_here (id bigint)`)
		require.NoError(t, err)
	}
}
