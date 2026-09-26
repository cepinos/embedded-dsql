// Package embeddeddsql starts a real PostgreSQL server for Go tests, without
// Docker, and fronts it with an in-process wire-protocol proxy that enforces
// the Aurora DSQL PostgreSQL-compatibility subset, so tests catch
// DSQL-incompatible SQL without needing a cluster.
//
//	db, err := embeddeddsql.Start(ctx, embeddeddsql.Options{})
//	if err != nil {
//		t.Fatal(err)
//	}
//	t.Cleanup(func() {
//		if err := db.Stop(); err != nil {
//			t.Error(err)
//		}
//	})
//	conn, err := pgx.Connect(ctx, db.DSN())
//
// The proxy is a Go port of MiniStack's pgproxy.py (MiniStack 1.5.9). It
// validates both the simple and the extended query protocol and answers with
// the SQLSTATEs and messages Aurora DSQL uses: the denylist (extensions,
// triggers, TRUNCATE, temporary tables, ...), supported column types,
// identity columns, the ALTER TABLE subset, DEFERRABLE, CACHE values, index
// rules (CREATE INDEX ASYNC only, at most 8 keys, 24 indexes per table),
// locking clauses, the one-DDL-per-transaction and no-DDL-with-DML rules,
// the 3,000-row / 10 MiB / 5-minute transaction limits, OC001 schema
// conflicts, and dropping a primary key column. CREATE INDEX ASYNC and
// ALTER TABLE ASYNC ... VALIDATE CONSTRAINT return a job id, and sys.jobs
// and sys.wait_for_job are emulated.
//
// Documented limitations, inherited from MiniStack:
//
//   - CREATE INDEX ASYNC / ALTER TABLE ASYNC sent over the extended protocol
//     run at Execute time on the connection's backend; a statement that is
//     parsed but never executed registers no job. Jobs complete synchronously.
//   - Type and name parsing is regular-expression work, not a SQL parser.
//     Statement splitting, comment stripping, parenthesis matching and the
//     locking-clause rule are lexer-aware (literals, dollar quotes, quoted
//     identifiers, nested comments); the rest is pattern matching.
//   - The locking-clause rule is syntactic: a share-mode clause that DSQL
//     would accept as a no-op (SELECT 1 FOR SHARE) is refused anyway.
//   - Foreign keys are enforced by PostgreSQL, so a write and a concurrent
//     change to the referenced key wait on a row lock where DSQL resolves
//     them optimistically and fails the loser with 40001.
//   - Transaction row counting is static (VALUES tuples only); INSERT ...
//     SELECT, UPDATE and DELETE row counts are not tracked.
//   - OC001 emulation is optimistic: the catalog version bumps when DDL is
//     forwarded, not when it commits.
//   - Isolation is PostgreSQL's: the default is READ COMMITTED, and
//     concurrency conflicts surface as PostgreSQL reports them (for example
//     40001 under REPEATABLE READ), not through DSQL's optimistic commit.
//
// Linux and macOS on amd64 and arm64 are supported.
package embeddeddsql
