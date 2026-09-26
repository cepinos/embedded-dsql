package embeddeddsql

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	instancePrefix = "embedded-dsql-"
	ownerFile      = "owner.pid"
)

// instanceLayout is the per-instance directory tree. embedded-postgres wipes
// its runtime path on start, so it gets a subdirectory and the owner marker
// lives next to it.
type instanceLayout struct {
	root string
}

func (l instanceLayout) runtime() string { return filepath.Join(l.root, "rt") }
func (l instanceLayout) data() string    { return filepath.Join(l.runtime(), "data") }
func (l instanceLayout) owner() string   { return filepath.Join(l.root, ownerFile) }

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	return !isZombie(pid)
}

// isZombie reports whether pid has exited but was never reaped. pg_ctl
// daemonizes the postmaster, so once it exits it is the orphan of whatever
// runs as PID 1; in a container whose PID 1 does not reap orphans it stays
// a zombie, which kill(pid, 0) still reports as alive. Without procfs
// (macOS) the kill check stands on its own.
func isZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The state follows the command name, which is parenthesised and may
	// itself contain spaces or parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 || end+2 >= len(data) {
		return false
	}
	state := data[end+2]
	return state == 'Z' || state == 'X'
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strconv.Atoi(strings.TrimSpace(line))
}

// postmasterPID returns the PID recorded by a running (or crashed) server in
// dataDir, or 0 when there is none.
func postmasterPID(dataDir string) (int, error) {
	pid, err := readPID(filepath.Join(dataDir, "postmaster.pid"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("embedded-dsql: read postmaster.pid in %s: %w", dataDir, err)
	}
	return pid, nil
}

// stopLeftover stops a server still running on dataDir, for example one
// left behind by a test binary that was killed. pg_ctl daemonizes the
// server, so it outlives the process that started it.
func stopLeftover(binDir, dataDir string, logw io.Writer) error {
	pid, err := postmasterPID(dataDir)
	if err != nil || pid == 0 || !processAlive(pid) {
		return err
	}
	cmd := exec.Command(filepath.Join(binDir, "bin", "pg_ctl"), "stop", "-m", "immediate", "-w", "-D", dataDir)
	cmd.Stdout, cmd.Stderr = logw, logw
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("embedded-dsql: stop leftover PostgreSQL (pid %d) in %s: %w", pid, dataDir, err)
	}
	return nil
}

// sweepStale stops and removes instances in the temp dir whose owning test
// process is gone. Directories without an owner marker are left alone for a
// while, in case their owner is still starting up.
func sweepStale(binDir string, logw io.Writer) error {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return fmt.Errorf("embedded-dsql: list %s: %w", os.TempDir(), err)
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), instancePrefix) || e.Name() == "embedded-dsql-root" {
			continue
		}
		layout := instanceLayout{root: filepath.Join(os.TempDir(), e.Name())}
		owner, err := readPID(layout.owner())
		switch {
		case err == nil && processAlive(owner):
			continue
		case err != nil:
			info, statErr := e.Info()
			if statErr != nil || time.Since(info.ModTime()) < 10*time.Minute {
				continue
			}
		}
		if err := stopLeftover(binDir, layout.data(), logw); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.RemoveAll(layout.root); err != nil {
			errs = append(errs, fmt.Errorf("embedded-dsql: remove stale %s: %w", layout.root, err))
		}
	}
	return errors.Join(errs...)
}

// newInstanceDir creates the instance directory. With an explicit dir it
// first stops whatever an earlier run left running there.
func newInstanceDir(explicit, binDir string, as *runAs, logw io.Writer) (instanceLayout, bool, error) {
	if explicit != "" {
		explicit, err := filepath.Abs(explicit)
		if err != nil {
			return instanceLayout{}, false, fmt.Errorf("embedded-dsql: resolve runtime dir: %w", err)
		}
		layout := instanceLayout{root: explicit}
		if err := os.MkdirAll(explicit, 0o755); err != nil {
			return layout, false, fmt.Errorf("embedded-dsql: create runtime dir %s: %w", explicit, err)
		}
		if as != nil {
			if err := os.Chmod(explicit, 0o755); err != nil {
				return layout, false, fmt.Errorf("embedded-dsql: chmod %s: %w", explicit, err)
			}
			if err := checkSearchable(filepath.Dir(explicit), as); err != nil {
				return layout, false, err
			}
		}
		if err := stopLeftover(binDir, layout.data(), logw); err != nil {
			return layout, false, err
		}
		return layout, false, writeOwner(layout)
	}
	if err := sweepStale(binDir, logw); err != nil {
		fmt.Fprintf(logw, "%v\n", err)
	}
	dir, err := os.MkdirTemp("", instancePrefix+"*")
	if err != nil {
		return instanceLayout{}, false, fmt.Errorf("embedded-dsql: create runtime dir: %w", err)
	}
	layout := instanceLayout{root: dir}
	if as != nil {
		// The unprivileged account must traverse into its runtime dir.
		if err := os.Chmod(dir, 0o755); err != nil {
			return layout, true, errors.Join(fmt.Errorf("embedded-dsql: chmod %s: %w", dir, err), os.RemoveAll(dir))
		}
	}
	if err := writeOwner(layout); err != nil {
		return layout, true, errors.Join(err, os.RemoveAll(dir))
	}
	return layout, true, nil
}

// checkSearchable reports whether the unprivileged account can traverse dir
// and every ancestor, which PostgreSQL needs to reach its data directory.
func checkSearchable(dir string, as *runAs) error {
	for d := dir; ; d = filepath.Dir(d) {
		info, err := os.Stat(d)
		if err != nil {
			return fmt.Errorf("embedded-dsql: stat %s: %w", d, err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		owned := ok && int(st.Uid) == as.uid
		if info.Mode().Perm()&0o001 == 0 && !owned {
			return fmt.Errorf("embedded-dsql: running as root, PostgreSQL runs as %s, which cannot reach RuntimeDir "+
				"through %s (not world-searchable); use a RuntimeDir under a searchable path or leave it empty", as.name, d)
		}
		if parent := filepath.Dir(d); parent == d {
			return nil
		}
	}
}

func writeOwner(l instanceLayout) error {
	if err := os.WriteFile(l.owner(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return fmt.Errorf("embedded-dsql: write %s: %w", l.owner(), err)
	}
	return nil
}

// freePort asks the kernel for a free loopback TCP port.
func freePort() (uint32, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("embedded-dsql: find a free port: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	closeErr := ln.Close()
	if !ok {
		return 0, errors.Join(errors.New("embedded-dsql: unexpected listener address"), closeErr)
	}
	return uint32(addr.Port), closeErr
}
