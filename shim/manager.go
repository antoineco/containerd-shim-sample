package shim

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/schedcore"
	"github.com/containerd/containerd/runtime/v2/shim"
)

// containerd-specific environment variables set while invoking the shim's
// start command.
// https://github.com/containerd/containerd/tree/v1.7.3/runtime/v2#start
const (
	contdShimEnvShedCore = "SCHED_CORE"
)

// Name of the file that contains the init pid.
const initPidFile = "init.pid"

// https://pubs.opengroup.org/onlinepubs/9699919799/utilities/V3_chap02.html#tag_18_21_18
const exitCodeSignal = 128

// NewManager returns a new shim.Manager.
func NewManager(name string) *manager {
	return &manager{name: name}
}

// manager manages shim processes.
type manager struct {
	name string
}

var _ shim.Manager = (*manager)(nil)

// Name returns the name of the shim.
func (m *manager) Name() string {
	return m.name
}

// Start starts a shim process.
// It implements the shim's "start" command.
// https://github.com/containerd/containerd/tree/v1.7.3/runtime/v2#start
func (*manager) Start(ctx context.Context, containerID string, opts shim.StartOpts) (addr string, retErr error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("getting executable of current process: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting current working directory: %w", err)
	}

	var args []string
	if opts.Debug {
		args = append(args, "-debug")
	}

	cmdCfg := &shim.CommandConfig{
		Runtime:      self,
		Address:      opts.Address,
		TTRPCAddress: opts.TTRPCAddress,
		Path:         cwd,
		SchedCore:    os.Getenv(contdShimEnvShedCore) != "",
		Args:         args,
	}

	cmd, err := shim.Command(ctx, cmdCfg)
	if err != nil {
		return "", fmt.Errorf("creating shim command: %w", err)
	}

	sockAddr, err := shim.SocketAddress(ctx, opts.Address, containerID)
	if err != nil {
		return "", fmt.Errorf("getting a socket address: %w", err)
	}

	socket, err := shim.NewSocket(sockAddr)
	if err != nil {
		switch {
		// the only time where this would happen is if there is a bug and the socket
		// was not cleaned up in the cleanup method of the shim or we are using the
		// grouping functionality where the new process should be run with the same
		// shim as an existing container
		case !shim.SocketEaddrinuse(err):
			return "", fmt.Errorf("creating new shim socket: %w", err)

		case shim.CanConnect(sockAddr):
			if err := shim.WriteAddress("address", sockAddr); err != nil {
				return "", fmt.Errorf("writing socket address file: %w", err)
			}
			return sockAddr, nil
		}

		if err := shim.RemoveSocket(sockAddr); err != nil {
			return "", fmt.Errorf("removing pre-existing shim socket: %w", err)
		}

		if socket, err = shim.NewSocket(sockAddr); err != nil {
			return "", fmt.Errorf("creating new shim socket (second attempt): %w", err)
		}
	}

	defer func() {
		if retErr != nil {
			if err := socket.Close(); err != nil {
				log.G(ctx).WithError(err).Error("failed to close shim socket on start error")
			}
			if err := shim.RemoveSocket(sockAddr); err != nil {
				log.G(ctx).WithError(err).Error("removing shim socket on start error")
			}
		}
	}()

	if err := shim.WriteAddress("address", sockAddr); err != nil {
		return "", fmt.Errorf("writing socket address file: %w", err)
	}

	sockF, err := socket.File()
	if err != nil {
		return "", fmt.Errorf("getting shim socket file descriptor: %w", err)
	}

	cmd.ExtraFiles = append(cmd.ExtraFiles, sockF)

	runtime.LockOSThread()

	if cmdCfg.SchedCore {
		if err := schedcore.Create(schedcore.ProcessGroup); err != nil {
			return "", fmt.Errorf("enabling sched core support: %w", err)
		}
	}

	if err := cmd.Start(); err != nil {
		sockF.Close()
		return "", fmt.Errorf("starting shim command: %w", err)
	}

	runtime.UnlockOSThread()

	defer func() {
		if retErr != nil {
			if err := cmd.Cancel(); err != nil {
				log.G(ctx).WithError(err).Error("failed to cancel shim command")
			}
		}
	}()

	go func() {
		if err := cmd.Wait(); err != nil {
			if _, ok := err.(*exec.ExitError); !ok {
				log.G(ctx).WithError(err).Errorf("failed to wait for shim process %d", cmd.Process.Pid)
			}
		}
	}()

	if err := shim.AdjustOOMScore(cmd.Process.Pid); err != nil {
		return "", fmt.Errorf("adjusting shim process OOM score: %w", err)
	}

	return sockAddr, nil
}

// Stop stops a shim process.
// It implements the shim's "delete" command.
// https://github.com/containerd/containerd/tree/v1.7.3/runtime/v2#delete
func (*manager) Stop(ctx context.Context, containerID string) (shim.StopStatus, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return shim.StopStatus{}, fmt.Errorf("getting current working directory: %w", err)
	}

	pidPath := filepath.Join(filepath.Join(filepath.Dir(cwd), containerID), initPidFile)
	pid, err := readPidFile(pidPath)
	if err != nil {
		log.G(ctx).WithError(err).Warn("failed to read init pid file")
	}

	if pid > 0 {
		p, _ := os.FindProcess(pid)
		// The POSIX standard specifies that a null-signal can be sent to check
		// whether a PID is valid.
		if err := p.Signal(syscall.Signal(0)); err == nil {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				log.G(ctx).WithError(err).Warnf("failed to send kill syscall to init process %d", pid)
			}
		}
	}

	return shim.StopStatus{
		Pid:        pid,
		ExitedAt:   time.Now(),
		ExitStatus: int(exitCodeSignal + syscall.SIGKILL),
	}, nil
}

// readPidFile reads the pid file at the provided path and returns the pid it
// contains.
func readPidFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(string(data))
}
