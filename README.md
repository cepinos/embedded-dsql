# embedded-dsql

Aurora DSQL-compatible PostgreSQL for Go tests: no Docker, no cluster.

`embedded-dsql` is a Go library that starts a real PostgreSQL 16 server via
[fergusstrange/embedded-postgres](https://github.com/fergusstrange/embedded-postgres)
and puts an in-process wire-protocol proxy in front of it, built on
[`github.com/jackc/pgx/v5/pgproto3`](https://pkg.go.dev/github.com/jackc/pgx/v5/pgproto3).
The proxy enforces Aurora DSQL's PostgreSQL-compatibility subset and emulates
`CREATE INDEX ASYNC` and `sys.jobs`, so your tests catch DSQL-incompatible SQL
before it reaches a real cluster.

## Status

Work in progress: nothing is implemented yet and the API may change.
No LICENSE file yet — it will be added before the first release.

## Planned API

```go
dsn, stop, err := embeddeddsql.Start(ctx, embeddeddsql.Options{})
if err != nil {
	t.Fatal(err)
}
defer stop()
// connect to dsn with pgx / database/sql as you would to DSQL
```

Sketch: `Start(ctx, Options) (dsn string, stop func() error, err error)`.

## Planned pieces

- **Pinned binary fetch** of the PostgreSQL binaries into embedded-postgres'
  cache, verified against pinned SHA-256 checksums.
- **Root handling** via wrapper binaries, so the server can run when tests
  execute as root (e.g. in CI containers).
- **DSQL proxy**: a Go port of MiniStack's MIT-licensed
  `ministack/core/pgproxy.py`. The port must retain MiniStack's copyright and
  license notice, as the upstream header requires, including for AI-assisted
  ports.
- **Parity test suite** that runs the same cases against MiniStack to keep the
  two implementations in agreement.
