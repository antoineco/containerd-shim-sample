package shim

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	taskv2 "github.com/containerd/containerd/api/runtime/task/v2"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/shutdown"
	"github.com/containerd/containerd/protobuf"
	ptypes "github.com/containerd/containerd/protobuf/types"
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/ttrpc"

	"shim-sample/io"
)

// newTaskService returns a new taskv2.TaskService.
func newTaskService(ss shutdown.Service) (*timePrintTaskService, error) {
	s := &timePrintTaskService{
		procs: make(initProcByTaskID, 1),
		ss:    ss,
	}

	sockAddr, err := shim.ReadAddress("address")
	if err != nil {
		return nil, fmt.Errorf("reading socket address from address file: %w", err)
	}

	ss.RegisterCallback(func(context.Context) error {
		if err := shim.RemoveSocket(sockAddr); err != nil {
			return fmt.Errorf("removing shim socket on shutdown: %w", err)
		}
		return nil
	})

	return s, nil
}

// initProcByTaskID maps init (parent) processes to their associated task by ID.
type initProcByTaskID map[string]*initProcess

// initProcess encapsulates information about an init (parent) process.
type initProcess struct {
	pid int

	doneCtx    context.Context
	exitTime   time.Time
	exitStatus int

	stdout string
}

// timePrintTaskService is an implementation of a containerd taskv2.TaskService
// which prints the current time at regular intervals.
type timePrintTaskService struct {
	m     sync.RWMutex
	procs initProcByTaskID

	ss shutdown.Service
}

var (
	_ shim.TTRPCService  = (*timePrintTaskService)(nil)
	_ taskv2.TaskService = (*timePrintTaskService)(nil)
)

// RegisterTTRPC registers this TTRPC service with the given TTRPC server.
func (s *timePrintTaskService) RegisterTTRPC(srv *ttrpc.Server) error {
	taskv2.RegisterTaskService(srv, s)
	return nil
}

// Create creates a new task and runs its init process.
func (s *timePrintTaskService) Create(ctx context.Context, r *taskv2.CreateTaskRequest) (_ *taskv2.CreateTaskResponse, retErr error) {
	log.G(ctx).Debugf("create id:%s", r.ID)

	s.m.Lock()
	defer s.m.Unlock()
	if _, ok := s.procs[r.ID]; ok {
		return nil, errdefs.ErrAlreadyExists
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getting current working directory: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c",
		"while date --rfc-3339=seconds; do "+
			"sleep 5; "+
			"done",
	)

	pio, err := io.NewPipeIO(r.Stdout)
	if err != nil {
		return nil, fmt.Errorf("creating pipe io for stdout %s: %w", r.Stdout, err)
	}

	go func() {
		if err := pio.Copy(ctx); err != nil {
			log.G(ctx).WithError(err).Warn("failed to copy from stdout pipe")
		}
	}()

	cmd.Stdout = pio.Writer()

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("running init command: %w", err)
	}

	defer func() {
		if retErr != nil {
			if err := pio.Close(); err != nil {
				log.G(ctx).WithError(err).Error("failed to close stdout pipe io")
			}
			if err := cmd.Cancel(); err != nil {
				log.G(ctx).WithError(err).Error("failed to cancel task init command")
			}
		}
	}()

	pid := cmd.Process.Pid

	doneCtx, markDone := context.WithCancel(context.Background())

	go func() {
		defer markDone()

		if err := cmd.Wait(); err != nil {
			if _, ok := err.(*exec.ExitError); !ok {
				log.G(ctx).WithError(err).Errorf("failed to wait for init process %d", pid)
			}
		}

		if err := pio.Close(); err != nil {
			log.G(ctx).WithError(err).Error("failed to close stdout pipe io")
		}

		exitStatus := 255

		if cmd.ProcessState != nil {
			switch unixWaitStatus := cmd.ProcessState.Sys().(syscall.WaitStatus); {
			case cmd.ProcessState.Exited():
				exitStatus = cmd.ProcessState.ExitCode()
			case unixWaitStatus.Signaled():
				exitStatus = exitCodeSignal + int(unixWaitStatus.Signal())
			}
		} else {
			log.G(ctx).Warn("init process wait returned without setting process state")
		}

		s.m.Lock()
		defer s.m.Unlock()

		proc, ok := s.procs[r.ID]
		if !ok {
			log.G(ctx).WithError(err).Errorf("failed to write final status of done init process: task was removed")
		}

		proc.exitTime = time.Now()
		proc.exitStatus = exitStatus
	}()

	// If containerd needs to resort to calling the shim's "delete" command to
	// clean things up, having the process' pid readable from a file is the
	// only way for it to know what init process is associated with the task.
	pidPath := filepath.Join(filepath.Join(filepath.Dir(cwd), r.ID), initPidFile)
	if err := shim.WritePidFile(pidPath, pid); err != nil {
		return nil, fmt.Errorf("writing pid file of init process: %w", err)
	}

	s.procs[r.ID] = &initProcess{
		pid:     pid,
		doneCtx: doneCtx,
		stdout:  r.Stdout,
	}

	return &taskv2.CreateTaskResponse{
		Pid: uint32(pid),
	}, nil
}

// Start starts the primary user process inside the task.
func (s *timePrintTaskService) Start(ctx context.Context, r *taskv2.StartRequest) (*taskv2.StartResponse, error) {
	log.G(ctx).Debugf("start id:%s execid:%s", r.ID, r.ExecID)

	// we do not support starting a previously stopped task, and the init
	// process was already started inside the Create RPC call, so we naively
	// return its stored PID
	s.m.RLock()
	defer s.m.RUnlock()
	proc, ok := s.procs[r.ID]
	if !ok {
		return nil, fmt.Errorf("task not created: %w", errdefs.ErrNotFound)
	}

	return &taskv2.StartResponse{
		Pid: uint32(proc.pid),
	}, nil
}

// Delete deletes a task.
func (s *timePrintTaskService) Delete(ctx context.Context, r *taskv2.DeleteRequest) (*taskv2.DeleteResponse, error) {
	log.G(ctx).Debugf("delete id:%s execid:%s", r.ID, r.ExecID)

	s.m.Lock()
	defer s.m.Unlock()
	proc, ok := s.procs[r.ID]
	if !ok {
		return nil, fmt.Errorf("task not created: %w", errdefs.ErrNotFound)
	}

	if proc.exitTime.IsZero() {
		return nil, errdefs.ToGRPCf(errdefs.ErrFailedPrecondition, "init process %d is not done yet", proc.pid)
	}

	delete(s.procs, r.ID)

	return &taskv2.DeleteResponse{
		Pid:        uint32(proc.pid),
		ExitStatus: uint32(proc.exitStatus),
		ExitedAt:   protobuf.ToTimestamp(proc.exitTime),
	}, nil
}

// Exec executes an additional process inside the task.
func (*timePrintTaskService) Exec(ctx context.Context, r *taskv2.ExecProcessRequest) (*ptypes.Empty, error) {
	log.G(ctx).Debugf("exec id:%s execid:%s", r.ID, r.ExecID)
	return nil, errdefs.ErrNotImplemented
}

// ResizePty resizes the pty of a process.
func (*timePrintTaskService) ResizePty(ctx context.Context, r *taskv2.ResizePtyRequest) (*ptypes.Empty, error) {
	log.G(ctx).Debugf("resizepty id:%s execid:%s", r.ID, r.ExecID)
	return nil, errdefs.ErrNotImplemented
}

// State returns the runtime state of a process.
func (s *timePrintTaskService) State(ctx context.Context, r *taskv2.StateRequest) (*taskv2.StateResponse, error) {
	log.G(ctx).Debugf("state id:%s execid:%s", r.ID, r.ExecID)

	s.m.RLock()
	defer s.m.RUnlock()
	proc, ok := s.procs[r.ID]
	if !ok {
		return nil, fmt.Errorf("task not created: %w", errdefs.ErrNotFound)
	}

	status := tasktypes.Status_RUNNING
	if !proc.exitTime.IsZero() {
		status = tasktypes.Status_STOPPED
	}

	return &taskv2.StateResponse{
		ID:         r.ID,
		Pid:        uint32(proc.pid),
		Status:     status,
		Stdout:     proc.stdout,
		ExitStatus: uint32(proc.exitStatus),
		ExitedAt:   protobuf.ToTimestamp(proc.exitTime),
	}, nil
}

// Pause pauses a task.
func (*timePrintTaskService) Pause(ctx context.Context, r *taskv2.PauseRequest) (*ptypes.Empty, error) {
	log.G(ctx).Debugf("pause id:%s", r.ID)
	return nil, errdefs.ErrNotImplemented
}

// Resume resumes a task.
func (*timePrintTaskService) Resume(ctx context.Context, r *taskv2.ResumeRequest) (*ptypes.Empty, error) {
	log.G(ctx).Debugf("resume id:%s", r.ID)
	return nil, errdefs.ErrNotImplemented
}

// Kill kills a process.
func (s *timePrintTaskService) Kill(ctx context.Context, r *taskv2.KillRequest) (*ptypes.Empty, error) {
	log.G(ctx).Debugf("kill id:%s execid:%s", r.ID, r.ExecID)

	s.m.RLock()
	defer s.m.RUnlock()
	proc, ok := s.procs[r.ID]
	if !ok {
		return nil, fmt.Errorf("task not created: %w", errdefs.ErrNotFound)
	}

	if proc.pid > 0 {
		p, _ := os.FindProcess(proc.pid)
		// The POSIX standard specifies that a null-signal can be sent to check
		// whether a PID is valid.
		if err := p.Signal(syscall.Signal(0)); err == nil {
			sig := syscall.Signal(r.Signal)
			if err := p.Signal(sig); err != nil {
				return nil, fmt.Errorf("sending %s to init process: %w", sig, err)
			}
		}
	}

	return &ptypes.Empty{}, nil
}

// Pids returns all pids inside a task.
func (s *timePrintTaskService) Pids(ctx context.Context, r *taskv2.PidsRequest) (*taskv2.PidsResponse, error) {
	log.G(ctx).Debugf("pids id:%s", r.ID)
	return nil, errdefs.ErrNotImplemented
}

// CloseIO closes the I/O of a process.
func (*timePrintTaskService) CloseIO(ctx context.Context, r *taskv2.CloseIORequest) (*ptypes.Empty, error) {
	log.G(ctx).Debugf("closeio id:%s execid:%s", r.ID, r.ExecID)
	return nil, errdefs.ErrNotImplemented
}

// Checkpoint creates a checkpoint of a task.
func (*timePrintTaskService) Checkpoint(ctx context.Context, r *taskv2.CheckpointTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).Debugf("checkpoint id:%s", r.ID)
	return nil, errdefs.ErrNotImplemented
}

// Connect returns the shim information of the underlying service.
func (s *timePrintTaskService) Connect(ctx context.Context, r *taskv2.ConnectRequest) (*taskv2.ConnectResponse, error) {
	log.G(ctx).Debugf("connect id:%s", r.ID)

	s.m.RLock()
	defer s.m.RUnlock()
	proc, ok := s.procs[r.ID]
	if !ok {
		return nil, fmt.Errorf("task not created: %w", errdefs.ErrNotFound)
	}

	return &taskv2.ConnectResponse{
		ShimPid: uint32(os.Getpid()),
		TaskPid: uint32(proc.pid),
	}, nil
}

// Shutdown is called after the underlying resources of the shim are cleaned up and the service can be stopped.
func (s *timePrintTaskService) Shutdown(ctx context.Context, r *taskv2.ShutdownRequest) (*ptypes.Empty, error) {
	log.G(ctx).Debugf("shutdown id:%s", r.ID)

	s.ss.Shutdown()
	return &ptypes.Empty{}, nil
}

// Stats returns container level system stats for a task and its processes.
func (*timePrintTaskService) Stats(ctx context.Context, r *taskv2.StatsRequest) (*taskv2.StatsResponse, error) {
	log.G(ctx).Debugf("stats id:%s", r.ID)
	return nil, errdefs.ErrNotImplemented
}

// Update updates the live task.
func (*timePrintTaskService) Update(ctx context.Context, r *taskv2.UpdateTaskRequest) (*ptypes.Empty, error) {
	log.G(ctx).Debugf("update id:%s", r.ID)
	return nil, errdefs.ErrNotImplemented
}

// Wait waits for a process to exit while attached to a task.
func (s *timePrintTaskService) Wait(ctx context.Context, r *taskv2.WaitRequest) (*taskv2.WaitResponse, error) {
	log.G(ctx).Debugf("wait id:%s execid:%s", r.ID, r.ExecID)

	doneCtx, err := func() (context.Context, error) {
		s.m.RLock()
		defer s.m.RUnlock()
		proc, ok := s.procs[r.ID]
		if !ok {
			return nil, fmt.Errorf("task not created: %w", errdefs.ErrNotFound)
		}
		return proc.doneCtx, nil
	}()
	if err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-doneCtx.Done():
	}

	s.m.RLock()
	defer s.m.RUnlock()
	proc, ok := s.procs[r.ID]
	if !ok {
		return nil, fmt.Errorf("task was removed: %w", errdefs.ErrNotFound)
	}

	return &taskv2.WaitResponse{
		ExitStatus: uint32(proc.exitStatus),
		ExitedAt:   protobuf.ToTimestamp(proc.exitTime),
	}, nil
}
