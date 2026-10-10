// Package process runs engine subprocesses without a shell and cancels their whole process group.
package process

import (
	"bufio"
	"bytes"
	"context"
	"errors"
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
func Run(ctx context.Context, bin string, args []string, env []string, onLine func(string)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	}
	wg.Add(2)
	go scan(stdout)
	go scan(stderr)
	wg.Wait()
	err = cmd.Wait()
	if ctx.Err() != nil {
		err = ctx.Err()
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
