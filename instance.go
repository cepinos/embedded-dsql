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
	return err == nil || errors.Is(err, syscall.EPERM)
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
		layout := instanceLayout{root: explicit}
		if err := os.MkdirAll(explicit, 0o755); err != nil {
			return layout, false, fmt.Errorf("embedded-dsql: create runtime dir %s: %w", explicit, err)
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
