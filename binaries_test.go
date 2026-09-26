package embeddeddsql

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallWrapperReExecsTheRealBinary(t *testing.T) {
	binDir := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	out := filepath.Join(t.TempDir(), "args")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + out + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "initdb"), []byte(fake), 0o755))

	// Switching to our own uid keeps the test runnable without root.
	as := &runAs{name: "self", uid: os.Getuid(), gid: os.Getgid(), exec: "env"}
	require.NoError(t, installWrapper(binDir, "initdb", as))

	runtime := t.TempDir()
	data := filepath.Join(runtime, "data")
	pwfile := filepath.Join(runtime, "pwfile")
	require.NoError(t, os.WriteFile(pwfile, []byte("secret"), 0o600))

	cmd := exec.Command(filepath.Join(binDir, "initdb"), "-A", "password", "-D", data, "--pwfile="+pwfile)
	combined, err := cmd.CombinedOutput()
	require.NoError(t, err, string(combined))

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, []string{"-A", "password", "-D", data, "--pwfile=" + pwfile},
		strings.Split(strings.TrimSpace(string(got)), "\n"))
	info, err := os.Stat(data)
	require.NoError(t, err)
	require.True(t, info.IsDir(), "the wrapper creates the data dir for the account")
}

func TestCheckSearchable(t *testing.T) {
	other := &runAs{name: "someone-else", uid: os.Getuid() + 12345}
	cases := []struct {
		name    string
		mode    os.FileMode
		wantErr bool
	}{
		{name: "world searchable", mode: 0o755},
		{name: "private", mode: 0o700, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "parent")
			require.NoError(t, os.Mkdir(dir, 0o755))
			for d := dir; d != filepath.Clean(os.TempDir()) && d != filepath.Dir(d); d = filepath.Dir(d) {
				require.NoError(t, os.Chmod(d, 0o755))
			}
			require.NoError(t, os.Chmod(dir, tc.mode))
			t.Cleanup(func() { require.NoError(t, os.Chmod(dir, 0o755)) })

			err := checkSearchable(dir, other)
			if tc.wantErr {
				require.ErrorContains(t, err, "not world-searchable")
				return
			}
			require.NoError(t, err)
		})
	}
}
