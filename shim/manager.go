package shim

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/schedcore"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/log"
)

// containerd-specific environment variables set while invoking the shim's
// start command.
// https://github.com/containerd/containerd/tree/v2.3.1/core/runtime/v2#start
const (
	contdShimEnvShedCore = "SCHED_CORE"
)

// Name of the file that contains the init pid.
const initPidFile = "init.pid"

// https://pubs.opengroup.org/onlinepubs/9699919799/utilities/V3_chap02.html#tag_18_21_18
const exitCodeSignal = 128

// NewManager returns a new shim manager.
func NewManager(name string) *manager {
	return &manager{name: name}
}

// manager manages shim processes.
type manager struct {
	name string
}

var _ shim.Shim = (*manager)(nil)

// Name returns the name of the shim.
func (m *manager) Name() string {
	return m.name
}

// Start starts a shim process.
// It implements the shim's "start" command.
// https://github.com/containerd/containerd/tree/v2.3.1/core/runtime/v2#start
func (*manager) Start(ctx context.Context, params *bootstrap.BootstrapParams) (_ *bootstrap.BootstrapResult, retErr error) {
	res := &bootstrap.BootstrapResult{
		Version:  3,
		Protocol: "ttrpc",
	}

	id := params.GetInstanceID()
	addr := params.GetContainerdGrpcAddress()
	debug := params.GetLogLevel() <= bootstrap.LogLevel_LOG_LEVEL_DEBUG

	cmd, err := newShimCommand(ctx, id, addr, debug)
	if err != nil {
		return nil, fmt.Errorf("creating shim command: %w", err)
	}

	sockRoot := params.GetSocketDir()
	if sockRoot == "" {
		sockRoot = filepath.Join(defaults.DefaultStateDir, "s") // ref. [shim.SocketAddress]
	}
	sockAddr, err := shim.CreateSocketAddress(ctx, sockRoot, addr, id, false)
	if err != nil {
		return nil, fmt.Errorf("getting a socket address: %w", err)
	}

	socket, err := shim.NewSocket(sockAddr)
	if err != nil {
		switch {
		// the only time where this would happen is if there is a bug and the socket
		// was not cleaned up in the cleanup method of the shim or we are using the
		// grouping functionality where the new process should be run with the same
		// shim as an existing container
		case !shim.SocketEaddrinuse(err):
			return nil, fmt.Errorf("creating new shim socket: %w", err)

		case shim.CanConnect(sockAddr):
			res.Address = sockAddr
			return res, nil
		}

		if err := shim.RemoveSocket(sockAddr); err != nil {
			return nil, fmt.Errorf("removing pre-existing shim socket: %w", err)
		}

		if socket, err = shim.NewSocket(sockAddr); err != nil {
			return nil, fmt.Errorf("creating new shim socket (second attempt): %w", err)
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

	sockF, err := socket.File()
	if err != nil {
		return nil, fmt.Errorf("getting shim socket file descriptor: %w", err)
	}

	cmd.ExtraFiles = append(cmd.ExtraFiles, sockF)

	runtime.LockOSThread()

	if os.Getenv(contdShimEnvShedCore) != "" {
		if err := schedcore.Create(schedcore.ProcessGroup); err != nil {
			return nil, fmt.Errorf("enabling sched core support: %w", err)
		}
	}

	if err := cmd.Start(); err != nil {
		sockF.Close()
		return nil, fmt.Errorf("starting shim command: %w", err)
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
		return nil, fmt.Errorf("adjusting shim process OOM score: %w", err)
	}

	res.Address = sockAddr
	return res, nil
}

// Stop stops a shim process.
// It implements the shim's "delete" command.
// https://github.com/containerd/containerd/tree/v2.3.1/core/runtime/v2#delete
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

// Info returns details about the runtime plugin.
// It implements the shim's "-info" flag.
// https://github.com/containerd/containerd/tree/v2.3.1/core/runtime/v2#-info
func (m *manager) Info(ctx context.Context, _ io.Reader) (*types.RuntimeInfo, error) {
	return &types.RuntimeInfo{
		Name: m.name,
	}, nil
}

// newShimCommand returns the shim command to be executed.
func newShimCommand(ctx context.Context, id, containerdAddress string, debug bool) (*exec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("getting executable of current process: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getting current working directory: %w", err)
	}
	args := []string{
		"-namespace", ns,
		"-id", id,
		"-address", containerdAddress,
	}
	if debug {
		args = append(args, "-debug")
	}
	cmd := exec.Command(self, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GOMAXPROCS=4")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	return cmd, nil
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
