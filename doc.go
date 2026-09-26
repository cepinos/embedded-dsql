// Package embeddeddsql starts a real PostgreSQL server for Go tests, without
// Docker, and fronts it with an in-process wire-protocol proxy that enforces
// the Aurora DSQL PostgreSQL-compatibility subset, so tests catch
// DSQL-incompatible SQL without needing a cluster.
package embeddeddsql
