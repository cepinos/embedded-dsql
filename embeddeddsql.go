package embeddeddsql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/cepinos/embedded-dsql/internal/pgproxy"
)

const (
	backendUser     = "postgres"
	backendPassword = "postgres"
	database        = "postgres"
	// clientUser and clientPassword are what the DSN carries. The proxy
	// accepts any credentials, the way a DSQL IAM token is just a password.
	clientUser     = "admin"
	clientPassword = "embedded-dsql"
	startAttempts  = 3
)

// Options configures Start. The zero value is ready to use.
type Options struct {
	// CacheDir holds the downloaded PostgreSQL archive and the extracted
	// binaries. Default: $GOMODCACHE/../embedded-postgres (for example
	// ~/go/pkg/embedded-postgres), falling back to the user cache dir.
	CacheDir string
	// RuntimeDir holds this instance's data directory. Default: a fresh
	// temporary directory, removed by Stop. An explicit RuntimeDir is reused:
	// Start stops any server an earlier run left running there.
	RuntimeDir string
	// Port is the loopback port of the DSN. 0 picks a free port.
	Port int
	// PostgresVersion is the zonky embedded-postgres-binaries version, any
	// version published to the repository. Default: DefaultPostgresVersion.
	// Versions with a pinned checksum are verified against the pin; others
	// against BinarySHA256 or the repository's <jar>.sha256 file.
	PostgresVersion string
	// BinaryRepositoryURL is the root of the Maven repository serving the
	// zonky jars (a Maven Central mirror or proxy). Default:
	// DefaultBinaryRepositoryURL. A trailing slash is ignored. Pinned
	// versions are still verified against the pin.
	BinaryRepositoryURL string
	// BinarySHA256 is the hex SHA-256 of the binaries jar. When set, the jar
	// is verified against it instead of the pinned checksum or the
	// repository's .sha256 file.
	BinarySHA256 string
	// Logger receives PostgreSQL's output and the proxy's connection errors.
	// Default: discarded.
	Logger io.Writer
	// Passthrough skips the DSQL proxy: DSN points straight at PostgreSQL
	// and nothing is validated. The default is strict DSQL validation.
	Passthrough bool
}

// DB is a running embedded DSQL instance.
type DB struct {
	pg          *embeddedpostgres.EmbeddedPostgres
	proxy       *pgproxy.Proxy
	dsn         string
	pgDSN       string
	layout      instanceLayout
	ownsDir     bool
	stopOnce    sync.Once
	stopErr     error
	postmasterP int
}

// Start downloads (once, verified by checksum) and starts a
// PostgreSQL server, then fronts it with the DSQL validation proxy. Call Stop
// when done; Stop leaves no process behind.
func Start(ctx context.Context, opts Options) (*DB, error) {
	logw := opts.Logger
	if logw == nil {
		logw = io.Discard
	}
	logf := func(format string, args ...any) { fmt.Fprintf(logw, format+"\n", args...) }

	art, err := resolveArtifact(opts)
	if err != nil {
		return nil, err
	}
	version := art.version
	cacheDir := opts.CacheDir
	if cacheDir == "" {
		if cacheDir, err = defaultCacheDir(); err != nil {
			return nil, err
		}
	}
	archive, archiveSHA256, err := ensureArchive(ctx, art, cacheDir, logf)
	if err != nil {
		return nil, err
	}
	as, err := detectRunAs()
	if err != nil {
		return nil, err
	}
	binDir, err := binariesDir(cacheDir, art, archiveSHA256, as)
	if err != nil {
		return nil, err
	}
	if err := ensureBinaries(archive, binDir, as); err != nil {
		return nil, err
	}
	layout, ownsDir, err := newInstanceDir(opts.RuntimeDir, binDir, as, logw)
	if err != nil {
		return nil, err
	}

	db := &DB{layout: layout, ownsDir: ownsDir}
	cleanup := func(cause error) error {
		if ownsDir {
			return errors.Join(cause, os.RemoveAll(layout.root))
		}
		return cause
	}

	var pgPort uint32
	var startErr error
	for attempt := 0; attempt < startAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, cleanup(err)
		}
		pgPort, err = choosePort(opts.Port, opts.Passthrough)
		if err != nil {
			return nil, cleanup(err)
		}
		cfg := embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.PostgresVersion(version)).
			Port(pgPort).
			Database(database).
			Username(backendUser).
			Password(backendPassword).
			CachePath(cacheDir).
			BinariesPath(binDir).
			RuntimePath(layout.runtime()).
			DataPath(layout.data()).
			StartTimeout(90 * time.Second).
			Logger(logw).
			StartParameters(map[string]string{
				"listen_addresses":        "127.0.0.1",
				"unix_socket_directories": "",
				"fsync":                   "off",
				"synchronous_commit":      "off",
				"full_page_writes":        "off",
			})
		db.pg = embeddedpostgres.NewDatabase(cfg)
		if startErr = db.pg.Start(); startErr == nil {
			break
		}
		// A lost port race or a slow machine; the next attempt picks a new
		// port and embedded-postgres starts from a clean runtime dir.
		if stopErr := stopLeftover(binDir, layout.data(), logw); stopErr != nil {
			startErr = errors.Join(startErr, stopErr)
			break
		}
		if opts.Port != 0 && opts.Passthrough {
			break
		}
	}
	if startErr != nil {
		return nil, cleanup(fmt.Errorf("embedded-dsql: start PostgreSQL %s from %s: %w", version, binDir, startErr))
	}
	if pid, err := postmasterPID(layout.data()); err == nil {
		db.postmasterP = pid
	}

	db.pgDSN = buildDSN(backendUser, backendPassword, int(pgPort))
	db.dsn = db.pgDSN
	if !opts.Passthrough {
		proxy, err := pgproxy.Listen(net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port)), pgproxy.Config{
			BackendHost:     "127.0.0.1",
			BackendPort:     uint16(pgPort),
			BackendUser:     backendUser,
			BackendPassword: backendPassword,
			Logger:          log.New(logw, "", log.LstdFlags),
		})
		if err != nil {
			return nil, cleanup(errors.Join(err, db.pg.Stop()))
		}
		db.proxy = proxy
		db.dsn = buildDSN(clientUser, clientPassword, proxy.Addr().Port)
	}
	return db, nil
}

func choosePort(requested int, passthrough bool) (uint32, error) {
	if passthrough && requested != 0 {
		if requested < 0 || requested > 65535 {
			return 0, fmt.Errorf("embedded-dsql: invalid port %d", requested)
		}
		return uint32(requested), nil
	}
	return freePort()
}

func buildDSN(user, password string, port int) string {
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}).String()
}

// DSN is the connection string clients use: the DSQL proxy, or PostgreSQL
// itself when Options.Passthrough is set. Any user and password are
// accepted by the proxy, so a DSQL IAM token works as the password.
func (db *DB) DSN() string { return db.dsn }

// PostgresDSN points straight at the backing PostgreSQL server, bypassing
// DSQL validation. Use it for test fixtures DSQL refuses, such as TRUNCATE
// between tests; statements sent this way do not update the proxy's
// transaction or catalog tracking.
func (db *DB) PostgresDSN() string { return db.pgDSN }

// Stop closes the proxy and every client connection, stops PostgreSQL and
// removes the temporary runtime directory. It is safe to call more than once.
func (db *DB) Stop() error {
	db.stopOnce.Do(func() {
		var errs []error
		if db.proxy != nil {
			errs = append(errs, db.proxy.Close())
		}
		if err := db.pg.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("embedded-dsql: stop PostgreSQL: %w", err))
		}
		if db.postmasterP != 0 {
			errs = append(errs, waitGone(db.postmasterP, 30*time.Second))
		}
		if db.ownsDir {
			errs = append(errs, os.RemoveAll(db.layout.root))
		}
		db.stopErr = errors.Join(errs...)
	})
	return db.stopErr
}

// waitGone waits for the postmaster to exit; pg_ctl stop -w returns once
// the server stopped accepting connections, which can be a moment earlier.
func waitGone(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			return fmt.Errorf("embedded-dsql: PostgreSQL (pid %d) still running after stop", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}
