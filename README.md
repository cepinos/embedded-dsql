# embedded-dsql

Aurora DSQL-compatible PostgreSQL for Go tests: no Docker, no cluster.

`embedded-dsql` starts a real PostgreSQL 16 server via
[fergusstrange/embedded-postgres](https://github.com/fergusstrange/embedded-postgres)
and puts an in-process wire-protocol proxy in front of it, built on
[`github.com/jackc/pgx/v5/pgproto3`](https://pkg.go.dev/github.com/jackc/pgx/v5/pgproto3).
The proxy enforces Aurora DSQL's PostgreSQL-compatibility subset and emulates
`CREATE INDEX ASYNC` and `sys.jobs`, so your tests catch DSQL-incompatible SQL
before it reaches a real cluster.

The proxy is a Go port of [MiniStack](https://github.com/ministackorg/ministack)'s
DSQL proxy, and a parity suite keeps the two in agreement.

## Install

```sh
go get github.com/cepinos/embedded-dsql
```

Linux and macOS on amd64 and arm64. The first `Start` downloads the PostgreSQL
binaries (about 15 MB) from Maven Central; later runs use the cache.

## Usage

```go
func TestMain(m *testing.M) {
	ctx := context.Background()
	db, err := embeddeddsql.Start(ctx, embeddeddsql.Options{})
	if err != nil {
		log.Fatal(err)
	}
	dsn = db.DSN()
	code := m.Run()
	if err := db.Stop(); err != nil {
		log.Print(err)
		code = 1
	}
	os.Exit(code)
}

func TestSomething(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, `CREATE TABLE t (id serial)`)
	// ERROR: type "serial" is not supported (SQLSTATE 0A000)

	_, err = conn.Exec(ctx, `CREATE TABLE t (id uuid PRIMARY KEY, name text)`)
	require.NoError(t, err)

	var jobID string
	err = conn.QueryRow(ctx, `CREATE INDEX ASYNC t_name ON t (name)`).Scan(&jobID)
	require.NoError(t, err)
	var done bool
	err = conn.QueryRow(ctx, `SELECT sys.wait_for_job($1)`, jobID).Scan(&done)
	require.NoError(t, err)
}
```

`database/sql` works the same way through `github.com/jackc/pgx/v5/stdlib`.

## API

```go
func Start(ctx context.Context, opts Options) (*DB, error)

func (db *DB) DSN() string         // postgres://... of the DSQL proxy
func (db *DB) PostgresDSN() string // the PostgreSQL server itself, unvalidated
func (db *DB) Stop() error         // idempotent; leaves no process behind

type Options struct {
	CacheDir        string    // default $GOMODCACHE/../embedded-postgres
	RuntimeDir      string    // default: a fresh temp dir, removed by Stop
	Port            int       // DSN port; 0 picks a free one
	PostgresVersion string    // default "16.14.0" (must have a pinned checksum)
	Logger          io.Writer // PostgreSQL output and proxy errors; default discarded
	Passthrough     bool      // no proxy: DSN points straight at PostgreSQL
}
```

The DSN uses user `admin` and database `postgres`. The proxy accepts any user
and password, so a DSQL IAM auth token works as the password. `PostgresDSN`
is there for fixtures DSQL refuses, such as `TRUNCATE` between tests;
statements sent that way bypass the proxy's transaction and catalog tracking.
Strict validation is the default; `Passthrough: true` turns the proxy off.

## What is enforced

Both the simple and the extended query protocol (pgx, `database/sql`) are
validated, with the SQLSTATEs and messages Aurora DSQL uses:

- **Denylist** (`0A000`): `CREATE DATABASE/TYPE/EXTENSION/TRIGGER/PROCEDURE/
  TABLESPACE/RULE/AGGREGATE/DOMAIN/CAST/COLLATION/OPERATOR/PUBLICATION/
  SUBSCRIPTION`, materialized views, temporary and unlogged tables, foreign
  data wrappers, `COPY`, `LISTEN/NOTIFY/UNLISTEN`, `TRUNCATE`, `DO`,
  `SAVEPOINT`, `LOCK`, `VACUUM`, `CLUSTER`, `REINDEX`, `ALTER SYSTEM`,
  prepared transactions, functions in any language but SQL, partitioning,
  inheritance and `EXCLUDE` constraints.
- **Column types**: only DSQL's types (no `serial`, arrays, `money`, `inet`,
  geometric types, ...); identity columns on `bigint` only; sequence and
  identity `CACHE` must be 1 or at least 65536.
- **ALTER TABLE subset**: no `ALTER COLUMN TYPE`, no `SET NOT NULL`, `ADD
  COLUMN` without constraints, `ADD CONSTRAINT` only for `CHECK`/`FOREIGN KEY
  ... NOT VALID` and `UNIQUE USING INDEX`, `ADD GENERATED ... AS IDENTITY`
  needs `CACHE`, and no dropping a primary key column.
- **DEFERRABLE** only on foreign keys.
- **Indexes**: `CREATE INDEX ASYNC` only (no `CONCURRENTLY`, `USING`, partial
  indexes), immutable expressions only (`42P17`), no expressions in
  `INCLUDE`, at most 8 key columns (`54011`) and 24 indexes per table.
- **Locking clauses**: only `FOR UPDATE` and `FOR KEY SHARE`.
- **Transactions** (`25006`): one DDL statement per transaction, no DDL mixed
  with DML, at most 3,000 rows, 10 MiB and 5 minutes. A statement rejected
  inside a transaction aborts it (`25P02` afterwards, `COMMIT` rolls back).
- **OC001**: a transaction whose connection has not seen another session's
  DDL fails with `40001`.
- **Async DDL**: `CREATE [UNIQUE] INDEX ASYNC` and `ALTER TABLE ASYNC ...
  VALIDATE CONSTRAINT` return a `job_id`; `sys.jobs` and `sys.wait_for_job`
  report it. Data that violates the constraint fails the job, not the
  statement, as on DSQL.

### Limitations

Inherited from MiniStack's proxy:

- Async DDL completes synchronously. Over the extended protocol it runs at
  `Execute`; a statement that is parsed but never executed registers no job.
- Parsing is regular-expression work, not a SQL parser. Statement splitting,
  comment stripping, parenthesis matching and the locking-clause rule are
  lexer-aware (literals, dollar quotes, quoted identifiers, nested
  comments); the rest is pattern matching.
- The locking-clause rule is syntactic: a share-mode clause DSQL would accept
  as a no-op (`SELECT 1 FOR SHARE`) is refused anyway.
- Foreign keys are enforced by PostgreSQL, so conflicting writes wait on a
  row lock where DSQL fails the loser optimistically with `40001`.
- Transaction row counting is static (`VALUES` tuples only); `INSERT ...
  SELECT`, `UPDATE` and `DELETE` row counts are not tracked.
- OC001 is optimistic: the catalog version bumps when DDL is forwarded, not
  when it commits.
- Isolation is PostgreSQL's (default `READ COMMITTED`); conflicts surface as
  PostgreSQL reports them, for example `40001` on a write conflict under
  `REPEATABLE READ`, not through DSQL's optimistic commit.
- The server announces `server_version` 16.4 like MiniStack, while the
  backend runs the pinned PostgreSQL version.

### Known differences from MiniStack

The parity suite (below) finds no difference on its case table. By design,
embedded-dsql goes further than MiniStack 1.5.9 in a few places, all outside
what the parity suite sends:

- Prepared statements that clients cache (pgx does) are re-checked at `Bind`
  against the transaction rules and a pending abort, not only at `Parse`.
- `sys.jobs` / `sys.wait_for_job` accept a bound parameter (`job_id = $1`,
  `sys.wait_for_job($1)`); their rows are computed at `Execute`, so a cached
  statement sees new jobs; a binary-format `bool` is encoded as binary.
- The primary-key probe uses `to_regclass`, so an unknown relation no longer
  aborts the client's transaction before the backend answers.
- Proxy probes wait until the backend owes the client nothing, so a
  pipelining client cannot have its results swallowed.
- Client startup parameters (`application_name`, `search_path` via
  `options`, ...) are forwarded to the backend, and a backend connection
  failure is reported to the client as `08006`.

## Binaries, cache and root

- The PostgreSQL binaries come from the zonky
  `embedded-postgres-binaries-<os>-<arch>` jar on Maven Central. Its SHA-256,
  and that of the `.txz` inside, are **pinned in code** for each supported
  platform; a mismatch fails loudly with the URL and path. Downloads use
  `net/http` defaults, so `HTTPS_PROXY` is honoured.
- The archive is written atomically, under a cross-process file lock, to
  `<CacheDir>/embedded-postgres-binaries-<os>-<arch>-<version>.txz`, which is
  where embedded-postgres looks, so it never downloads by itself. A corrupt
  cached archive is replaced. Binaries are extracted once to
  `<CacheDir>/extracted/`. The default `CacheDir` sits next to the Go module
  cache, which CI caches usually persist; the included workflow caches it
  explicitly.
- Every instance gets free ports and its own runtime directory, so parallel
  test binaries never collide. Stop waits for the server to exit. Start stops
  a server an earlier run left in an explicit `RuntimeDir`, and removes
  temporary instances whose test process died without calling Stop.
- PostgreSQL refuses to run as root. When the tests run as root (a CI
  container), the binaries are extracted to a root-owned directory under the
  temp dir and `initdb`/`pg_ctl` are replaced by wrapper scripts that run the
  real binaries as `nobody` via `setpriv` or `runuser` (util-linux, present
  on the Debian-based `golang` images). Without either tool Start fails with
  a clear error. An explicit `RuntimeDir` must then be reachable by `nobody`.
- embedded-postgres keeps a small log file per start in the temp dir.

## Parity suite

`parity_test.go` (build tag `parity`) runs one SQL case table, over both wire
protocols, against a MiniStack DSQL cluster and against embedded-dsql, and
requires identical outcomes: SQLSTATE and message, or command tag and rows.

```sh
docker run -d --name ministack -p 4566:4566 -p 25432-25461:25432-25461 \
  -e SERVICES=dsql -e DSQL_STRICT=1 \
  -v /var/run/docker.sock:/var/run/docker.sock ministackorg/ministack:1.5.9
EMBEDDED_DSQL_MINISTACK_URL=http://127.0.0.1:4566 go test -tags parity -run Parity -v .
```

The suite creates a cluster through MiniStack's DSQL API and deletes it
afterwards. Set `EMBEDDED_DSQL_PARITY_VERBOSE=1` to log every outcome.

## Ported files

These files are ports of MiniStack **1.5.9** (`ministackorg/ministack:1.5.9`,
PyPI `ministack==1.5.9`) and keep MiniStack's copyright notice:

| File | Ported from |
| --- | --- |
| `internal/pgproxy/lexer.go` | `ministack/core/pgproxy.py` |
| `internal/pgproxy/validate.go` | `ministack/core/pgproxy.py` |
| `internal/pgproxy/jobs.go` | `ministack/core/pgproxy.py` |
| `internal/pgproxy/proxy.go` | `ministack/core/pgproxy.py` |
| `internal/pgproxy/validate_test.go` | `tests/test_dsql.py` |
| `parity_test.go` (case table) | `tests/test_dsql.py` |

MiniStack's MIT license is reproduced in [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES).

## License

MIT — see [LICENSE](LICENSE). Files ported from MiniStack keep MiniStack's own
MIT copyright notice.
