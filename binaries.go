package embeddeddsql

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/xi2/xz"
)

const completeMarker = ".embedded-dsql-complete"

// runAs is the unprivileged account PostgreSQL runs as when the tests run as
// root, which PostgreSQL refuses.
type runAs struct {
	name     string
	uid, gid int
	// exec is the shell command prefix that re-executes a program as the
	// account, for example "setpriv --reuid=65534 ...  --".
	exec string
}

// detectRunAs returns nil when the process is not root. As root it picks the
// "nobody" account and a tool able to switch to it.
func detectRunAs() (*runAs, error) {
	if os.Geteuid() != 0 {
		return nil, nil
	}
	u, err := user.Lookup("nobody")
	if err != nil {
		return nil, fmt.Errorf("embedded-dsql: running as root needs an unprivileged \"nobody\" account for PostgreSQL: %w", err)
	}
	uid, uidErr := strconv.Atoi(u.Uid)
	gid, gidErr := strconv.Atoi(u.Gid)
	if err := errors.Join(uidErr, gidErr); err != nil {
		return nil, fmt.Errorf("embedded-dsql: parse ids of account nobody: %w", err)
	}
	r := &runAs{name: u.Username, uid: uid, gid: gid}
	if path, err := exec.LookPath("setpriv"); err == nil {
		r.exec = fmt.Sprintf("%s --reuid=%d --regid=%d --clear-groups --", path, uid, gid)
		return r, nil
	}
	if path, err := exec.LookPath("runuser"); err == nil {
		r.exec = fmt.Sprintf("%s -u %s --", path, u.Username)
		return r, nil
	}
	return nil, errors.New("embedded-dsql: running as root needs setpriv or runuser (util-linux) " +
		"to start PostgreSQL as an unprivileged user; install util-linux or run the tests as a non-root user")
}

// binariesDir is where the extracted PostgreSQL binaries live. As root they go
// to a root-owned directory under the system temp dir, so the unprivileged
// account can reach them (the cache usually sits under /root).
func binariesDir(cacheDir string, a artifact, as *runAs) (string, error) {
	if as == nil {
		return filepath.Join(cacheDir, "extracted", a.name()), nil
	}
	parent := filepath.Join(os.TempDir(), "embedded-dsql-root")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("embedded-dsql: create %s: %w", parent, err)
	}
	if err := checkRootOwnedDir(parent); err != nil {
		return "", err
	}
	return filepath.Join(parent, fmt.Sprintf("%s-uid%d", a.name(), as.uid)), nil
}

func checkRootOwnedDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("embedded-dsql: stat %s: %w", dir, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || st.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("embedded-dsql: %s must be a directory owned by root and not group/world writable", dir)
	}
	return nil
}

// ensureBinaries extracts the archive into dir once, under a cross-process
// lock, so embedded-postgres finds bin/pg_ctl and never extracts itself. As
// root it also installs the initdb and pg_ctl wrappers.
func ensureBinaries(archive, dir string, as *runAs) error {
	if _, err := os.Stat(filepath.Join(dir, completeMarker)); err == nil {
		return nil
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("embedded-dsql: create %s: %w", parent, err)
	}
	unlock, err := lockFile(filepath.Join(parent, ".embedded-dsql.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(dir, completeMarker)); err == nil {
		return nil
	}

	tmp, err := os.MkdirTemp(parent, ".extract-*")
	if err != nil {
		return fmt.Errorf("embedded-dsql: create extraction dir in %s: %w", parent, err)
	}
	if err := populateBinaries(archive, tmp, as); err != nil {
		return errors.Join(err, os.RemoveAll(tmp))
	}
	if err := os.RemoveAll(dir); err != nil {
		return errors.Join(fmt.Errorf("embedded-dsql: remove stale %s: %w", dir, err), os.RemoveAll(tmp))
	}
	if err := os.Rename(tmp, dir); err != nil {
		return errors.Join(fmt.Errorf("embedded-dsql: move binaries into %s: %w", dir, err), os.RemoveAll(tmp))
	}
	return nil
}

func populateBinaries(archive, dir string, as *runAs) error {
	if err := os.Chmod(dir, 0o755); err != nil {
		return fmt.Errorf("embedded-dsql: chmod %s: %w", dir, err)
	}
	if err := extractTxz(archive, dir); err != nil {
		return fmt.Errorf("embedded-dsql: extract %s into %s: %w", archive, dir, err)
	}
	if as != nil {
		for _, name := range []string{"initdb", "pg_ctl"} {
			if err := installWrapper(filepath.Join(dir, "bin"), name, as); err != nil {
				return err
			}
		}
	}
	marker := filepath.Join(dir, completeMarker)
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return fmt.Errorf("embedded-dsql: write %s: %w", marker, err)
	}
	return nil
}

func extractTxz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	extractErr := extractTar(f, dest)
	return errors.Join(extractErr, f.Close())
}

func extractTar(r io.Reader, dest string) error {
	xzr, err := xz.NewReader(r, 0)
	if err != nil {
		return err
	}
	tr := tar.NewReader(xzr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q escapes the destination", hdr.Name)
		}
		target := filepath.Join(dest, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFromTar(target, tr, os.FileMode(hdr.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			if err := os.Link(filepath.Join(dest, filepath.Clean(hdr.Linkname)), target); err != nil {
				return err
			}
		}
	}
}

func writeFromTar(target string, r io.Reader, perm os.FileMode) error {
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm|0o444)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, r)
	return errors.Join(copyErr, out.Close())
}

// wrapperScript re-executes the real binary as the unprivileged account.
// embedded-postgres runs initdb and pg_ctl with a bare exec.Command, and
// PostgreSQL refuses to run as root. The script first hands the data
// directory, its parent and the password file to the account.
const wrapperScript = `#!/bin/sh
# Generated by embedded-dsql: PostgreSQL refuses to run as root, so this
# re-executes the real %[1]s as %[2]s.
set -e
prev=""
for arg in "$@"; do
	case "$arg" in
	--pwfile=*) chown %[3]d:%[4]d "${arg#--pwfile=}" ;;
	esac
	if [ "$prev" = "-D" ]; then
		mkdir -p "$arg"
		chown %[3]d:%[4]d "$(dirname "$arg")" "$arg"
	fi
	prev="$arg"
done
cd /
exec %[5]s "$(dirname "$0")/%[1]s.real" "$@"
`

func installWrapper(binDir, name string, as *runAs) error {
	bin := filepath.Join(binDir, name)
	if err := os.Rename(bin, bin+".real"); err != nil {
		return fmt.Errorf("embedded-dsql: move %s aside: %w", bin, err)
	}
	script := fmt.Sprintf(wrapperScript, name, as.name, as.uid, as.gid, as.exec)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		return fmt.Errorf("embedded-dsql: write wrapper %s: %w", bin, err)
	}
	return nil
}
