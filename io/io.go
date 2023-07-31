package io

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/containerd/fifo"
)

// PipeIO can copy data from an anonymous pipe p into a named pipe dst.
// The anonymous pipe is meant to be connected to a container's stdout stream,
// whereas the named pipe is managed by containerd.
type PipeIO struct {
	p   *pipe
	dst string
}

// NewPipeIO creates an anonymous pipe for copying data into the dst named pipe.
func NewPipeIO(dst string) (*PipeIO, error) {
	p, err := newPipe()
	if err != nil {
		return nil, fmt.Errorf("creating pipe: %w", err)
	}

	return &PipeIO{
		p:   p,
		dst: dst,
	}, nil
}

// Copy continuously copies data from pio's anonymous pipe (read end) to its
// dst pipe, until any of them gets closed.
func (pio *PipeIO) Copy(ctx context.Context) error {
	ok, err := fifo.IsFifo(pio.dst)
	if err != nil {
		return fmt.Errorf("checking whether file %s is a fifo: %w", pio.dst, err)
	}
	if !ok {
		return fmt.Errorf("file %s is not a fifo", pio.dst)
	}

	var fw io.WriteCloser
	var fr io.Closer

	if fw, err = fifo.OpenFifo(ctx, pio.dst, syscall.O_WRONLY, 0); err != nil {
		return fmt.Errorf("opening write only fifo %s: %w", pio.dst, err)
	}
	defer fw.Close()

	// the read end of the dst pipe needs to remain open to avoid "broken pipe"
	// in detached mode
	if fr, err = fifo.OpenFifo(ctx, pio.dst, syscall.O_RDONLY, 0); err != nil {
		return fmt.Errorf("opening read only fifo %s: %w", pio.dst, err)
	}
	defer fr.Close()

	b := make([]byte, 4096)
	if _, err := io.CopyBuffer(fw, pio.p.r, b); err != nil {
		return fmt.Errorf("copying pipe data to destination: %w", err)
	}

	return nil
}

// Writer returns a writer to pio's anonymous pipe.
func (pio *PipeIO) Writer() io.Writer {
	return pio.p.w
}

// Close closes pio's anonymous pipe.
func (pio *PipeIO) Close() error {
	return pio.p.Close()
}

// pipe is a connected pair of files (anonymous pipe).
type pipe struct {
	r *os.File
	w *os.File
}

// newPipe creates a pipe.
func newPipe() (*pipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("creating os pipe: %w", err)
	}

	return &pipe{
		r: r,
		w: w,
	}, nil
}

// Close closes both ends (files) of the pipe.
func (p *pipe) Close() error {
	return errors.Join(p.w.Close(), p.r.Close())
}
