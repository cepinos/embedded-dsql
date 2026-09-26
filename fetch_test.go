package embeddeddsql

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func fakeJar(t *testing.T, txz []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("postgres-linux-x86_64.txz")
	require.NoError(t, err)
	_, err = w.Write(txz)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func fakeRepo(t *testing.T, jar []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, err := w.Write(jar)
		if err != nil {
			t.Errorf("write jar: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestEnsureArchive(t *testing.T) {
	txz := []byte("not really xz, but pinned")
	jar := fakeJar(t, txz)

	cases := []struct {
		name    string
		jarSum  string
		txzSum  string
		wantErr string
	}{
		{name: "verified download is cached", jarSum: sum(jar), txzSum: sum(txz)},
		{name: "jar checksum mismatch", jarSum: sum([]byte("other")), txzSum: sum(txz), wantErr: "checksum mismatch"},
		{name: "txz checksum mismatch", jarSum: sum(jar), txzSum: sum([]byte("other")), wantErr: "checksum mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := fakeRepo(t, jar)
			a := artifact{goos: "linux", arch: "amd64", version: "0.0.1", repo: srv.URL,
				sums: checksum{jar: tc.jarSum, txz: tc.txzSum}}
			cache := t.TempDir()

			path, err := ensureArchive(context.Background(), a, cache, t.Logf)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.ErrorContains(t, err, srv.URL)
				_, statErr := os.Stat(a.cachePath(cache))
				require.ErrorIs(t, statErr, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			require.Equal(t, filepath.Join(cache, "embedded-postgres-binaries-linux-amd64-0.0.1.txz"), path)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, txz, got)

			// A verified cache hit never downloads again.
			_, err = ensureArchive(context.Background(), a, cache, t.Logf)
			require.NoError(t, err)
			require.Equal(t, int32(1), hits.Load())
		})
	}
}

func TestEnsureArchiveReplacesACorruptCache(t *testing.T) {
	txz := []byte("pinned archive")
	jar := fakeJar(t, txz)
	srv, hits := fakeRepo(t, jar)
	a := artifact{goos: "linux", arch: "amd64", version: "0.0.1", repo: srv.URL,
		sums: checksum{jar: sum(jar), txz: sum(txz)}}
	cache := t.TempDir()
	require.NoError(t, os.WriteFile(a.cachePath(cache), []byte("truncated"), 0o644))

	path, err := ensureArchive(context.Background(), a, cache, t.Logf)
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, txz, got)
	require.Equal(t, int32(1), hits.Load())
}

func TestResolveArtifactRejectsUnpinnedVersions(t *testing.T) {
	_, err := resolveArtifact("16.0.0")
	require.ErrorContains(t, err, "no pinned checksum")

	a, err := resolveArtifact(DefaultPostgresVersion)
	require.NoError(t, err)
	require.Contains(t, a.url(), "https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-")
}

func TestDefaultCacheDir(t *testing.T) {
	t.Setenv("GOMODCACHE", "/x/go/pkg/mod")
	dir, err := defaultCacheDir()
	require.NoError(t, err)
	require.Equal(t, "/x/go/pkg/embedded-postgres", dir)

	t.Setenv("GOMODCACHE", "")
	t.Setenv("GOPATH", "/y/gopath")
	dir, err = defaultCacheDir()
	require.NoError(t, err)
	require.Equal(t, "/y/gopath/pkg/embedded-postgres", dir)
}
