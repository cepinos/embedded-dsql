package embeddeddsql

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultPostgresVersion is the PostgreSQL version started when
// Options.PostgresVersion is empty.
const DefaultPostgresVersion = "16.14.0"

// mavenRepository hosts the zonky embedded-postgres-binaries jars that
// github.com/fergusstrange/embedded-postgres consumes.
const mavenRepository = "https://repo1.maven.org/maven2"

// checksum pins one binaries jar and the .txz archive inside it.
type checksum struct {
	jar string
	txz string
}

// pinnedChecksums maps "<os>-<arch>-<version>" (zonky naming) to the SHA-256
// of the jar downloaded from Maven Central and of the .txz it contains.
var pinnedChecksums = map[string]checksum{
	"linux-amd64-16.14.0": {
		jar: "3278331b124b46fb9f8bea30c7ff3bd6227a3934dad38a6ddb625eb7dcfa8ca8",
		txz: "77eac54dd8e936ca817420c59c6251e5a07c1ad140941270999c18126c027c02",
	},
	"linux-arm64v8-16.14.0": {
		jar: "482f5ccdb15f68f4bfe524a5b9df734e169db9d1d29c32a513af44ac9efc127a",
		txz: "5883cd9540dd138ff594463b705d164b23bbfb650468a232aa2730371728f9fe",
	},
	"linux-amd64-alpine-16.14.0": {
		jar: "314dd372c506fa00dec20c192598b5fbb85338d6eab288fdbe56d9e91b01eea9",
		txz: "5586b86b53195ecb7214e9054e65a8cb62c3dc14a6ce287d6e6b23aa7766f640",
	},
	"linux-arm64v8-alpine-16.14.0": {
		jar: "a545ed0a8602d70c064e1c31f0164d39f7756e1aec03057e3ae8799346945605",
		txz: "43cc78ac761171cf9b6bc0df40db4782979c462a77bd1a25ed82cfabcaef3ab2",
	},
	"darwin-amd64-16.14.0": {
		jar: "9d082281befccc05f6c25dc0dd196382871543422f3ce850fb46cb035d38616a",
		txz: "e3f9962ad77632aa99901aa68ecfef878c49e916c337804408f0037deeb136e6",
	},
	"darwin-arm64v8-16.14.0": {
		jar: "d5d84a7e5103ade5557898fcf51affae69778be559bf9ba44819714c0d68e541",
		txz: "bc34c59637702d73d7bad7e17620c33be6fd219a28609eb26fdb36b85e6f89fd",
	},
}

// platform returns the zonky os and arch names for this machine, matching
// embedded-postgres' own version strategy.
func platform() (string, string, error) {
	goos, arch := runtime.GOOS, runtime.GOARCH
	switch {
	case goos == "linux" && arch == "amd64":
	case goos == "linux" && arch == "arm64":
		arch = "arm64v8"
	case goos == "darwin" && arch == "amd64":
	case goos == "darwin" && arch == "arm64":
		arch = "arm64v8"
	default:
		return "", "", fmt.Errorf("embedded-dsql: unsupported platform %s/%s (supported: linux and darwin on amd64 and arm64)", goos, arch)
	}
	if goos == "linux" {
		if _, err := os.Stat("/etc/alpine-release"); err == nil {
			arch += "-alpine"
		}
	}
	return goos, arch, nil
}

// artifact describes the PostgreSQL binaries for one platform and version.
type artifact struct {
	goos, arch, version string
	sums                checksum
	repo                string
}

func resolveArtifact(version string) (artifact, error) {
	goos, arch, err := platform()
	if err != nil {
		return artifact{}, err
	}
	key := goos + "-" + arch + "-" + version
	sums, ok := pinnedChecksums[key]
	if !ok {
		known := make([]string, 0, len(pinnedChecksums))
		for k := range pinnedChecksums {
			known = append(known, k)
		}
		return artifact{}, fmt.Errorf("embedded-dsql: no pinned checksum for PostgreSQL binaries %q (pinned: %s)",
			key, strings.Join(known, ", "))
	}
	return artifact{goos: goos, arch: arch, version: version, sums: sums, repo: mavenRepository}, nil
}

func (a artifact) name() string {
	return fmt.Sprintf("embedded-postgres-binaries-%s-%s-%s", a.goos, a.arch, a.version)
}

func (a artifact) url() string {
	return fmt.Sprintf("%s/io/zonky/test/postgres/embedded-postgres-binaries-%s-%s/%s/%s.jar",
		a.repo, a.goos, a.arch, a.version, a.name())
}

// cachePath is where embedded-postgres looks for the archive in cacheDir.
func (a artifact) cachePath(cacheDir string) string {
	return filepath.Join(cacheDir, a.name()+".txz")
}

// defaultCacheDir is $GOMODCACHE/../embedded-postgres, next to the module
// cache CI systems already persist, falling back to the user cache dir.
func defaultCacheDir() (string, error) {
	modCache := os.Getenv("GOMODCACHE")
	if modCache == "" {
		gopath := os.Getenv("GOPATH")
		if gopath != "" {
			gopath = filepath.SplitList(gopath)[0]
		} else if home, err := os.UserHomeDir(); err == nil {
			gopath = filepath.Join(home, "go")
		}
		if gopath != "" {
			modCache = filepath.Join(gopath, "pkg", "mod")
		}
	}
	if modCache != "" {
		return filepath.Join(filepath.Dir(modCache), "embedded-postgres"), nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("embedded-dsql: cannot determine a cache directory, set Options.CacheDir: %w", err)
	}
	return filepath.Join(dir, "embedded-postgres"), nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// cachedArchiveValid reports whether the archive in the cache matches its
// pinned checksum.
func cachedArchiveValid(path, want string) (bool, error) {
	got, err := sha256File(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("embedded-dsql: read cached archive %s: %w", path, err)
	}
	return got == want, nil
}

// ensureArchive makes sure the verified .txz is in cacheDir, downloading it
// under a cross-process lock when it is missing or does not match its pin.
// It returns the archive path.
func ensureArchive(ctx context.Context, a artifact, cacheDir string, logf func(string, ...any)) (string, error) {
	path := a.cachePath(cacheDir)
	ok, err := cachedArchiveValid(path, a.sums.txz)
	if err != nil || ok {
		return path, err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("embedded-dsql: create cache dir %s: %w", cacheDir, err)
	}
	unlock, err := lockFile(filepath.Join(cacheDir, ".embedded-dsql.lock"))
	if err != nil {
		return "", err
	}
	defer unlock()

	// Another process may have fetched it while this one waited.
	if ok, err := cachedArchiveValid(path, a.sums.txz); err != nil || ok {
		return path, err
	}
	logf("embedded-dsql: downloading %s to %s", a.url(), path)
	txz, err := download(ctx, a)
	if err != nil {
		return "", err
	}
	if err := writeAtomic(path, txz, 0o644); err != nil {
		return "", fmt.Errorf("embedded-dsql: write %s: %w", path, err)
	}
	return path, nil
}

var httpClient = &http.Client{Timeout: 10 * time.Minute}

// download fetches the jar, checks it against the pinned checksum and returns
// the verified .txz inside it.
func download(ctx context.Context, a artifact) ([]byte, error) {
	url := a.url()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("embedded-dsql: build request for %s: %w", url, err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedded-dsql: download %s: %w", url, err)
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("embedded-dsql: download %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedded-dsql: download %s: HTTP %s", url, resp.Status)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != a.sums.jar {
		return nil, fmt.Errorf("embedded-dsql: checksum mismatch for %s: got sha256 %s, pinned %s", url, got, a.sums.jar)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("embedded-dsql: open jar %s: %w", url, err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.HasSuffix(f.Name, ".txz") {
			continue
		}
		txz, err := readZipFile(f)
		if err != nil {
			return nil, fmt.Errorf("embedded-dsql: extract %s from %s: %w", f.Name, url, err)
		}
		sum := sha256.Sum256(txz)
		if got := hex.EncodeToString(sum[:]); got != a.sums.txz {
			return nil, fmt.Errorf("embedded-dsql: checksum mismatch for %s in %s: got sha256 %s, pinned %s", f.Name, url, got, a.sums.txz)
		}
		return txz, nil
	}
	return nil, fmt.Errorf("embedded-dsql: no .txz archive in %s", url)
}

func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(rc)
	return data, errors.Join(readErr, rc.Close())
}

// writeAtomic writes data to a temporary file next to path and renames it
// into place, so readers never see a partial file.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	_, writeErr := tmp.Write(data)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}
