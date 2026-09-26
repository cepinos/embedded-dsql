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
	"sort"
	"strings"
	"time"
)

// DefaultPostgresVersion is the PostgreSQL version started when
// Options.PostgresVersion is empty.
const DefaultPostgresVersion = "16.14.0"

// DefaultBinaryRepositoryURL is the Maven repository the PostgreSQL binaries
// come from when Options.BinaryRepositoryURL is empty: Maven Central, which
// hosts the zonky embedded-postgres-binaries jars that
// github.com/fergusstrange/embedded-postgres consumes.
const DefaultBinaryRepositoryURL = "https://repo1.maven.org/maven2"

// maxChecksumFileSize bounds the <jar>.sha256 response; a real one is a
// 64-character hex digest, optionally followed by a file name.
const maxChecksumFileSize = 4096

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

// artifact describes the PostgreSQL binaries for one platform and version,
// and how the downloaded jar is verified.
type artifact struct {
	goos, arch, version string
	// repo is the Maven repository root, without a trailing slash.
	repo string
	// jarSHA256 is the expected SHA-256 of the jar. Empty means it is read
	// from <jar URL>.sha256 in the same repository.
	jarSHA256 string
	// jarSource names where jarSHA256 comes from, for error messages.
	jarSource string
	// txzSHA256 is the pinned SHA-256 of the .txz inside the jar; empty for
	// versions without a pin or when Options.BinarySHA256 is set.
	txzSHA256 string
}

// resolveArtifact applies the defaults of opts and picks the integrity rule:
// Options.BinarySHA256, else the pinned checksums, else the repository's own
// .sha256 file.
func resolveArtifact(opts Options) (artifact, error) {
	goos, arch, err := platform()
	if err != nil {
		return artifact{}, err
	}
	a := artifact{goos: goos, arch: arch, version: opts.PostgresVersion, repo: opts.BinaryRepositoryURL}
	if a.version == "" {
		a.version = DefaultPostgresVersion
	}
	if a.repo == "" {
		a.repo = DefaultBinaryRepositoryURL
	}
	a.repo = strings.TrimRight(a.repo, "/")

	if opts.BinarySHA256 != "" {
		want := strings.ToLower(strings.TrimSpace(opts.BinarySHA256))
		if !isSHA256Hex(want) {
			return artifact{}, fmt.Errorf("embedded-dsql: Options.BinarySHA256 %q is not a hex SHA-256 digest", opts.BinarySHA256)
		}
		a.jarSHA256, a.jarSource = want, "Options.BinarySHA256"
		return a, nil
	}
	if sums, ok := pinnedChecksums[goos+"-"+arch+"-"+a.version]; ok {
		a.jarSHA256, a.txzSHA256, a.jarSource = sums.jar, sums.txz, "the pinned checksum"
	}
	return a, nil
}

func isSHA256Hex(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
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

// recordPath is the verification record kept next to the cached archive: the
// SHA-256 of the jar it came from and of the archive itself. It lets a
// version without a pinned archive checksum be reused without a download,
// and makes a changed expected jar checksum invalidate the cache.
func (a artifact) recordPath(cacheDir string) string {
	return a.cachePath(cacheDir) + ".embedded-dsql"
}

type verification struct {
	jar, txz string
}

func (v verification) encode() []byte {
	return []byte("jar " + v.jar + "\ntxz " + v.txz + "\n")
}

func readVerification(path string) (verification, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return verification{}, false, nil
	}
	if err != nil {
		return verification{}, false, fmt.Errorf("embedded-dsql: read %s: %w", path, err)
	}
	var v verification
	for _, line := range strings.Split(string(data), "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "jar":
			v.jar = value
		case "txz":
			v.txz = value
		}
	}
	return v, isSHA256Hex(v.jar) && isSHA256Hex(v.txz), nil
}

// cachedArchive reports whether the archive in the cache can be used, and
// its SHA-256. A pinned archive checksum is authoritative; otherwise the
// archive must match its verification record, and that record's jar must be
// the expected one when an expected jar checksum is known.
func cachedArchive(a artifact, cacheDir string) (string, bool, error) {
	path := a.cachePath(cacheDir)
	got, err := sha256File(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("embedded-dsql: read cached archive %s: %w", path, err)
	}
	if a.txzSHA256 != "" {
		return got, got == a.txzSHA256, nil
	}
	v, ok, err := readVerification(a.recordPath(cacheDir))
	if err != nil || !ok {
		return "", false, err
	}
	if v.txz != got || (a.jarSHA256 != "" && v.jar != a.jarSHA256) {
		return "", false, nil
	}
	return got, true, nil
}

// ensureArchive makes sure a verified .txz is in cacheDir, downloading it
// under a cross-process lock when it is missing or cannot be trusted. It
// returns the archive path and its SHA-256.
func ensureArchive(ctx context.Context, a artifact, cacheDir string, logf func(string, ...any)) (string, string, error) {
	path := a.cachePath(cacheDir)
	txzSum, ok, err := cachedArchive(a, cacheDir)
	if err != nil {
		return "", "", err
	}
	if ok {
		return path, txzSum, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", "", fmt.Errorf("embedded-dsql: create cache dir %s: %w", cacheDir, err)
	}
	unlock, err := lockFile(filepath.Join(cacheDir, ".embedded-dsql.lock"))
	if err != nil {
		return "", "", err
	}
	defer unlock()

	// Another process may have fetched it while this one waited.
	if txzSum, ok, err := cachedArchive(a, cacheDir); err != nil || ok {
		return path, txzSum, err
	}
	logf("embedded-dsql: downloading %s to %s", a.url(), path)
	txz, v, err := download(ctx, a)
	if err != nil {
		return "", "", err
	}
	// The archive goes first: a crash before the record is written leaves a
	// mismatch, which the next start re-downloads.
	if err := writeAtomic(path, txz, 0o644); err != nil {
		return "", "", fmt.Errorf("embedded-dsql: write %s: %w", path, err)
	}
	if err := writeAtomic(a.recordPath(cacheDir), v.encode(), 0o644); err != nil {
		return "", "", fmt.Errorf("embedded-dsql: write %s: %w", a.recordPath(cacheDir), err)
	}
	return path, v.txz, nil
}

var httpClient = &http.Client{Timeout: 10 * time.Minute}

// get fetches url and returns its body, reading at most limit bytes when
// limit is positive.
func get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", url, err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	var r io.Reader = resp.Body
	if limit > 0 {
		r = io.LimitReader(resp.Body, limit)
	}
	body, readErr := io.ReadAll(r)
	closeErr := resp.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return body, nil
}

// remoteJarChecksum reads <jar URL>.sha256 from the repository. Any failure
// is fatal: a version without a checksum is never used unverified.
func remoteJarChecksum(ctx context.Context, a artifact) (string, error) {
	url := a.url() + ".sha256"
	body, err := get(ctx, url, maxChecksumFileSize)
	if err == nil {
		fields := strings.Fields(string(body))
		if len(fields) > 0 && isSHA256Hex(strings.ToLower(fields[0])) {
			return strings.ToLower(fields[0]), nil
		}
		err = errors.New("not a hex SHA-256 digest")
	}
	return "", fmt.Errorf("embedded-dsql: cannot verify PostgreSQL %s binaries: checksum file %s: %w; "+
		"set Options.BinarySHA256 to the jar's SHA-256 or use a pinned version (%s)",
		a.version, url, err, strings.Join(pinnedVersions(a.goos, a.arch), ", "))
}

// pinnedVersions lists the versions with pinned checksums for a platform.
func pinnedVersions(goos, arch string) []string {
	prefix := goos + "-" + arch + "-"
	var versions []string
	for key := range pinnedChecksums {
		if v, ok := strings.CutPrefix(key, prefix); ok && !strings.Contains(v, "-") {
			versions = append(versions, v)
		}
	}
	sort.Strings(versions)
	return versions
}

// download fetches the jar, checks it against the expected checksum and
// returns the verified .txz inside it with its verification record.
func download(ctx context.Context, a artifact) ([]byte, verification, error) {
	want, source := a.jarSHA256, a.jarSource
	if want == "" {
		var err error
		if want, err = remoteJarChecksum(ctx, a); err != nil {
			return nil, verification{}, err
		}
		source = a.url() + ".sha256"
	}
	url := a.url()
	body, err := get(ctx, url, 0)
	if err != nil {
		return nil, verification{}, fmt.Errorf("embedded-dsql: download %s: %w", url, err)
	}
	if got := sha256Hex(body); got != want {
		return nil, verification{}, fmt.Errorf("embedded-dsql: checksum mismatch for %s: got sha256 %s, want %s from %s", url, got, want, source)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, verification{}, fmt.Errorf("embedded-dsql: open jar %s: %w", url, err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.HasSuffix(f.Name, ".txz") {
			continue
		}
		txz, err := readZipFile(f)
		if err != nil {
			return nil, verification{}, fmt.Errorf("embedded-dsql: extract %s from %s: %w", f.Name, url, err)
		}
		got := sha256Hex(txz)
		if a.txzSHA256 != "" && got != a.txzSHA256 {
			return nil, verification{}, fmt.Errorf("embedded-dsql: checksum mismatch for %s in %s: got sha256 %s, pinned %s", f.Name, url, got, a.txzSHA256)
		}
		return txz, verification{jar: want, txz: got}, nil
	}
	return nil, verification{}, fmt.Errorf("embedded-dsql: no .txz archive in %s", url)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
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
