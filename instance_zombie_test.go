package embeddeddsql

import (
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProcessAliveTreatsAnUnreapedChildAsGone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zombie detection reads /proc")
	}

	c := require.New(t)

	cmd := exec.Command("true")
	c.NoError(cmd.Start())

	pid := cmd.Process.Pid

	c.Eventually(func() bool { return isZombie(pid) }, 5*time.Second, 10*time.Millisecond,
		"the exited, unreaped child must show up as a zombie")
	c.False(processAlive(pid), "a zombie is not a running PostgreSQL")

	c.NoError(cmd.Wait())
	c.False(processAlive(pid))
}

func TestProcessAliveSeesARunningChild(t *testing.T) {
	c := require.New(t)

	cmd := exec.Command("sleep", "5")
	c.NoError(cmd.Start())

	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil {
			t.Logf("killing sleep: %v", err)
		}
		if err := cmd.Wait(); err != nil {
			t.Logf("sleep exited: %v", err)
		}
	})

	c.True(processAlive(cmd.Process.Pid))
	c.False(isZombie(cmd.Process.Pid))
}
