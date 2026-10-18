// Package process runs engine subprocesses without a shell and cancels their whole process group.
package process

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type lockedBuffer struct {
	sync.Mutex
	b     bytes.Buffer
	limit int
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	n := len(p)
	if b.limit > 0 {
		if len(p) >= b.limit {
			b.b.Reset()
			p = p[len(p)-b.limit:]
		} else if b.b.Len()+len(p) > b.limit {
			b.b.Next(b.b.Len() + len(p) - b.limit)
		}
	}
	_, err := b.b.Write(p)
	return n, err
}
func (b *lockedBuffer) Bytes() []byte {
	b.Lock()
	defer b.Unlock()
	return append([]byte(nil), b.b.Bytes()...)
}

func Output(ctx context.Context, bin string, args []string, env []string) ([]byte, error) {
	return Run(ctx, bin, args, env, nil)
}

// ErrOutputLimitExceeded indicates that a subprocess exceeded the combined
// stdout and stderr limit. OutputLimited stops the entire process group as
// soon as this happens, retaining at most the requested number of bytes.
var ErrOutputLimitExceeded = errors.New("process output limit exceeded")

var ErrOutputLineTooLong = errors.New("process output line exceeds 4 MiB")

type boundedOutput struct {
	sync.Mutex
	buffer   bytes.Buffer
	maximum  int
	exceeded bool
	cancel   context.CancelFunc
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	if b.exceeded {
		return 0, ErrOutputLimitExceeded
	}
	remaining := b.maximum - b.buffer.Len()
	if len(p) > remaining {
		_, _ = b.buffer.Write(p[:remaining])
		b.exceeded = true
		b.cancel()
		return remaining, ErrOutputLimitExceeded
	}
	return b.buffer.Write(p)
}

// OutputLimited bounds the total output before returning it to a caller. This
// is suitable for small untrusted metadata; stdout and stderr share one budget.
func OutputLimited(ctx context.Context, bin string, args []string, env []string, maxBytes int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, errors.New("process output limit must be greater than zero")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := processCommand(runCtx, bin, args, env)
	output := &boundedOutput{maximum: maxBytes, cancel: cancel}
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	output.Lock()
	data := append([]byte(nil), output.buffer.Bytes()...)
	exceeded := output.exceeded
	output.Unlock()
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if exceeded {
		err = ErrOutputLimitExceeded
	}
	return data, err
}

func processCommand(ctx context.Context, bin string, args []string, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func Run(ctx context.Context, bin string, args []string, env []string, onLine func(string)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := processCommand(ctx, bin, args, env)
	var output lockedBuffer
	if onLine == nil {
		cmd.Stdout = &output
		cmd.Stderr = &output
		err := cmd.Run()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return output.Bytes(), err
	}
	output.limit = 4 * 1024 * 1024
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	var callbacks sync.Mutex
	var scanOnce sync.Once
	var scanErr error
	scan := func(reader io.Reader) {
		defer wg.Done()
		s := bufio.NewScanner(reader)
		s.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for s.Scan() {
			line := s.Text()
			output.Write(append(append([]byte(nil), s.Bytes()...), '\n'))
			callbacks.Lock()
			onLine(line)
			callbacks.Unlock()
		}
		if err := s.Err(); err != nil {
			scanOnce.Do(func() { scanErr = err })
		}
		_, _ = io.Copy(io.Discard, reader)
	}
	wg.Add(2)
	go scan(stdout)
	go scan(stderr)
	wg.Wait()
	err = cmd.Wait()
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if err == nil && scanErr != nil {
		err = fmt.Errorf("%w: %v", ErrOutputLineTooLong, scanErr)
	}
	return output.Bytes(), err
}
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *exec.ExitError
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return -1
}
