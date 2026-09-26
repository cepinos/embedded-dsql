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
	"strings"
	"sync"
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

// mavenRepo is an httptest server acting as a Maven repository: it serves
// the bodies in files by URL path and 404s everything else.
type mavenRepo struct {
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
}

func newMavenRepo(t *testing.T) *mavenRepo {
	t.Helper()
	r := &mavenRepo{files: map[string][]byte{}, hits: map[string]int{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits[req.URL.Path]++
		body, ok := r.files[req.URL.Path]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		if _, err := w.Write(body); err != nil {
			t.Errorf("write %s: %v", req.URL.Path, err)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *mavenRepo) put(path string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[path] = body
}

func (r *mavenRepo) hitCount(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[path]
}

func (r *mavenRepo) totalHits() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.hits {
		n += c
	}
	return n
}

// jarPath is the zonky jar's path below the repository root.
func jarPath(goos, arch, version string) string {
	return "/io/zonky/test/postgres/embedded-postgres-binaries-" + goos + "-" + arch + "/" + version +
		"/embedded-postgres-binaries-" + goos + "-" + arch + "-" + version + ".jar"
}

func testArtifact(repo, version, jarSum, txzSum string) artifact {
	return artifact{goos: "linux", arch: "amd64", version: version, repo: repo,
		jarSHA256: jarSum, jarSource: "the pinned checksum", txzSHA256: txzSum}
}

func requireNoArchive(t *testing.T, cache string) {
	t.Helper()
	entries, err := os.ReadDir(cache)
	require.NoError(t, err)
	for _, e := range entries {
		require.NotContains(t, e.Name(), ".txz", "cache must hold no archive after a failed download")
	}
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
			repo := newMavenRepo(t)
			repo.put(jarPath("linux", "amd64", "0.0.1"), jar)
			a := testArtifact(repo.srv.URL, "0.0.1", tc.jarSum, tc.txzSum)
			cache := t.TempDir()

			path, txzSum, err := ensureArchive(context.Background(), a, cache, t.Logf)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.ErrorContains(t, err, repo.srv.URL)
				requireNoArchive(t, cache)
				return
			}
			require.NoError(t, err)
			require.Equal(t, sum(txz), txzSum)
			require.Equal(t, filepath.Join(cache, "embedded-postgres-binaries-linux-amd64-0.0.1.txz"), path)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, txz, got)

			// A verified cache hit never downloads again.
			_, _, err = ensureArchive(context.Background(), a, cache, t.Logf)
			require.NoError(t, err)
			require.Equal(t, 1, repo.totalHits())
		})
	}
}

func TestEnsureArchiveReplacesACorruptCache(t *testing.T) {
	txz := []byte("pinned archive")
	jar := fakeJar(t, txz)
	repo := newMavenRepo(t)
	repo.put(jarPath("linux", "amd64", "0.0.1"), jar)
	a := testArtifact(repo.srv.URL, "0.0.1", sum(jar), sum(txz))
	cache := t.TempDir()
	require.NoError(t, os.WriteFile(a.cachePath(cache), []byte("truncated"), 0o644))

	path, _, err := ensureArchive(context.Background(), a, cache, t.Logf)
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, txz, got)
	require.Equal(t, 1, repo.totalHits())
}

func TestResolveArtifact(t *testing.T) {
	goos, arch, err := platform()
	require.NoError(t, err)
	pin, pinned := pinnedChecksums[goos+"-"+arch+"-"+DefaultPostgresVersion]
	require.True(t, pinned, "the default version must be pinned for this platform")
	override := strings.Repeat("ab", 32)

	cases := []struct {
		name       string
		opts       Options
		wantURL    string
		wantJar    string
		wantTxz    string
		wantErr    string
		wantSource string
	}{
		{
			name:    "defaults use Maven Central and the pin",
			opts:    Options{},
			wantURL: "https://repo1.maven.org/maven2" + jarPath(goos, arch, DefaultPostgresVersion),
			wantJar: pin.jar, wantTxz: pin.txz, wantSource: "pinned",
		},
		{
			name:    "custom repository keeps the pin",
			opts:    Options{BinaryRepositoryURL: "https://mirror.example/maven2"},
			wantURL: "https://mirror.example/maven2" + jarPath(goos, arch, DefaultPostgresVersion),
			wantJar: pin.jar, wantTxz: pin.txz, wantSource: "pinned",
		},
		{
			name:    "trailing slashes are tolerated",
			opts:    Options{BinaryRepositoryURL: "https://mirror.example/maven2//"},
			wantURL: "https://mirror.example/maven2" + jarPath(goos, arch, DefaultPostgresVersion),
			wantJar: pin.jar, wantTxz: pin.txz, wantSource: "pinned",
		},
		{
			name:    "unpinned version is accepted with no checksum yet",
			opts:    Options{PostgresVersion: "16.15.0"},
			wantURL: "https://repo1.maven.org/maven2" + jarPath(goos, arch, "16.15.0"),
		},
		{
			name:    "BinarySHA256 replaces the pin",
			opts:    Options{BinarySHA256: strings.ToUpper(override)},
			wantURL: "https://repo1.maven.org/maven2" + jarPath(goos, arch, DefaultPostgresVersion),
			wantJar: override, wantSource: "Options.BinarySHA256",
		},
		{
			name:    "BinarySHA256 must be hex SHA-256",
			opts:    Options{BinarySHA256: "abc"},
			wantErr: "Options.BinarySHA256",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := resolveArtifact(tc.opts)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantURL, a.url())
			require.Equal(t, tc.wantJar, a.jarSHA256)
			require.Equal(t, tc.wantTxz, a.txzSHA256)
			require.Contains(t, a.jarSource, tc.wantSource)
		})
	}
}

func TestEnsureArchiveUsesTheConfiguredRepository(t *testing.T) {
	goos, arch, err := platform()
	require.NoError(t, err)
	txz := []byte("unpinned archive")
	jar := fakeJar(t, txz)
	repo := newMavenRepo(t)
	path := "/custom/maven2" + jarPath(goos, arch, "99.1.0")
	repo.put(path, jar)
	repo.put(path+".sha256", []byte(sum(jar)+"  embedded-postgres-binaries.jar\n"))

	for _, base := range []string{repo.srv.URL + "/custom/maven2", repo.srv.URL + "/custom/maven2/"} {
		t.Run(base, func(t *testing.T) {
			a, err := resolveArtifact(Options{BinaryRepositoryURL: base, PostgresVersion: "99.1.0"})
			require.NoError(t, err)
			cache := t.TempDir()
			before := repo.hitCount(path)

			archive, txzSum, err := ensureArchive(context.Background(), a, cache, t.Logf)
			require.NoError(t, err)
			require.Equal(t, filepath.Join(cache, "embedded-postgres-binaries-"+goos+"-"+arch+"-99.1.0.txz"), archive)
			require.Equal(t, sum(txz), txzSum)
			require.Equal(t, before+1, repo.hitCount(path))
		})
	}
}

func TestEnsureArchiveUnpinnedVersion(t *testing.T) {
	txz := []byte("unpinned archive")
	jar := fakeJar(t, txz)
	path := jarPath("linux", "amd64", "99.1.0")

	cases := []struct {
		name       string
		sha256     []byte // nil: no .sha256 file in the repository
		wantErr    []string
		jarFetched bool // whether the jar may be requested
	}{
		{name: "verified via the repository's .sha256", sha256: []byte(sum(jar) + "\n")},
		{name: "bare uppercase hex", sha256: []byte(strings.ToUpper(sum(jar)))},
		{
			name:    "missing .sha256 fails loudly",
			wantErr: []string{path + ".sha256", "404", "99.1.0", "Options.BinarySHA256", "pinned version"},
		},
		{
			name:    "malformed .sha256 fails loudly",
			sha256:  []byte("<html>not a checksum</html>"),
			wantErr: []string{path + ".sha256", "99.1.0", "Options.BinarySHA256", "pinned version"},
		},
		{
			name:       ".sha256 that does not match the jar",
			sha256:     []byte(sum([]byte("other jar"))),
			wantErr:    []string{"checksum mismatch", path + ".sha256"},
			jarFetched: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMavenRepo(t)
			repo.put(path, jar)
			if tc.sha256 != nil {
				repo.put(path+".sha256", tc.sha256)
			}
			a := testArtifact(repo.srv.URL, "99.1.0", "", "")
			cache := t.TempDir()

			archive, txzSum, err := ensureArchive(context.Background(), a, cache, t.Logf)
			if tc.wantErr != nil {
				for _, want := range tc.wantErr {
					require.ErrorContains(t, err, want)
				}
				requireNoArchive(t, cache)
				if !tc.jarFetched {
					require.Zero(t, repo.hitCount(path), "the jar must not be downloaded without a checksum")
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, sum(txz), txzSum)
			got, err := os.ReadFile(archive)
			require.NoError(t, err)
			require.Equal(t, txz, got)

			// The verification record makes the next start a cache hit.
			_, _, err = ensureArchive(context.Background(), a, cache, t.Logf)
			require.NoError(t, err)
			require.Equal(t, 1, repo.hitCount(path))
			require.Equal(t, 1, repo.hitCount(path+".sha256"))
		})
	}
}

func TestEnsureArchiveBinarySHA256(t *testing.T) {
	txz := []byte("archive checked by the caller's hash")
	jar := fakeJar(t, txz)
	path := jarPath("linux", "amd64", "99.1.0")

	cases := []struct {
		name    string
		want    string
		wantErr string
	}{
		{name: "matching hash is accepted", want: sum(jar)},
		{name: "wrong hash is rejected", want: sum([]byte("other")), wantErr: "Options.BinarySHA256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMavenRepo(t)
			repo.put(path, jar)
			// A .sha256 that disagrees proves the override is what is checked.
			repo.put(path+".sha256", []byte(sum([]byte("wrong"))))
			a, err := resolveArtifact(Options{PostgresVersion: "99.1.0", BinaryRepositoryURL: repo.srv.URL, BinarySHA256: tc.want})
			require.NoError(t, err)
			a.goos, a.arch = "linux", "amd64"
			cache := t.TempDir()

			_, txzSum, err := ensureArchive(context.Background(), a, cache, t.Logf)
			require.Zero(t, repo.hitCount(path+".sha256"))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, "checksum mismatch")
				require.ErrorContains(t, err, tc.wantErr)
				requireNoArchive(t, cache)
				return
			}
			require.NoError(t, err)
			require.Equal(t, sum(txz), txzSum)
		})
	}
}

func TestEnsureArchiveCacheFollowsTheExpectedJar(t *testing.T) {
	txzA, txzB := []byte("archive A"), []byte("archive B")
	jarA, jarB := fakeJar(t, txzA), fakeJar(t, txzB)
	path := jarPath("linux", "amd64", "99.1.0")
	repo := newMavenRepo(t)
	cache := t.TempDir()

	repo.put(path, jarA)
	a := testArtifact(repo.srv.URL, "99.1.0", sum(jarA), "")
	_, gotSum, err := ensureArchive(context.Background(), a, cache, t.Logf)
	require.NoError(t, err)
	require.Equal(t, sum(txzA), gotSum)

	// A different expected jar for the same version never reuses archive A.
	repo.put(path, jarB)
	b := testArtifact(repo.srv.URL, "99.1.0", sum(jarB), "")
	archive, gotSum, err := ensureArchive(context.Background(), b, cache, t.Logf)
	require.NoError(t, err)
	require.Equal(t, sum(txzB), gotSum)
	got, err := os.ReadFile(archive)
	require.NoError(t, err)
	require.Equal(t, txzB, got)
	require.Equal(t, 2, repo.hitCount(path))

	// Another version has its own cache file.
	other := testArtifact(repo.srv.URL, "99.2.0", sum(jarB), "")
	require.NotEqual(t, archive, other.cachePath(cache))
}

func TestPinnedVersionFromACustomRepositoryIsCheckedAgainstThePin(t *testing.T) {
	goos, arch, err := platform()
	require.NoError(t, err)
	jar := fakeJar(t, []byte("not the real PostgreSQL"))
	repo := newMavenRepo(t)
	path := jarPath(goos, arch, DefaultPostgresVersion)
	repo.put(path, jar)
	// The repository vouches for its own jar; the pin must still win.
	repo.put(path+".sha256", []byte(sum(jar)))
	a, err := resolveArtifact(Options{BinaryRepositoryURL: repo.srv.URL + "/"})
	require.NoError(t, err)
	cache := t.TempDir()

	_, _, err = ensureArchive(context.Background(), a, cache, t.Logf)
	require.ErrorContains(t, err, "checksum mismatch")
	require.ErrorContains(t, err, "pinned")
	require.ErrorContains(t, err, repo.srv.URL+path)
	require.Zero(t, repo.hitCount(path+".sha256"))
	requireNoArchive(t, cache)
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
