package process

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProcessHelper(t *testing.T) {
	if os.Getenv("VP_PROCESS_HELPER") != "1" {
		return
	}
	switch os.Getenv("VP_HELPER_MODE") {
	case "exit3":
		os.Exit(3)
	case "wait":
		time.Sleep(30 * time.Second)
	case "output":
		_, _ = os.Stdout.WriteString("out-data")
		_, _ = os.Stderr.WriteString("err-data")
	case "environment":
		_, _ = os.Stdout.WriteString(strings.Join(os.Environ(), "\n"))
	case "exact-output":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 64))
	case "too-long":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 1024*1024))
		time.Sleep(30 * time.Second)
	case "stderr-too-long":
		_, _ = os.Stderr.WriteString(strings.Repeat("x", 1024*1024))
		time.Sleep(30 * time.Second)
	case "oversized-line":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 5*1024*1024))
		_, _ = os.Stdout.WriteString("tail\n")
		_, _ = os.Stderr.WriteString(strings.Repeat("y", 128*1024) + "\n")
	}
	os.Exit(0)
}

func TestRunDrainsOversizedLines(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	var lines []string
	data, err := Run(ctx, os.Args[0], []string{"-test.run=TestProcessHelper"}, []string{"VP_PROCESS_HELPER=1", "VP_HELPER_MODE=oversized-line"}, func(line string) { lines = append(lines, line) })
	if !errors.Is(err, ErrOutputLineTooLong) || ctx.Err() != nil || time.Since(start) >= 5*time.Second {
		t.Fatalf("oversized line did not drain promptly: %v (context: %v)", err, ctx.Err())
	}
	if strings.Contains(string(data), "x") || len(lines) != 1 || lines[0] != strings.Repeat("y", 128*1024) {
		t.Fatal("partial oversized line reached output or callback")
	}
}

func TestProcessEnvironmentIsAllowlisted(t *testing.T) {
	for name, value := range map[string]string{"RCLONE_DRY_RUN": "true", "RESTIC_PASSWORD": "x", "VP_OAUTH_CLIENT_SECRET": "s", "HTTPS_PROXY": "http://p", "LC_ALL": "C", "https_proxy": "http://lower"} {
		t.Setenv(name, value)
	}
	data, err := Output(context.Background(), os.Args[0], []string{"-test.run=TestProcessHelper"}, []string{"VP_PROCESS_HELPER=1", "VP_HELPER_MODE=environment", "RCLONE_CONFIG=/x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"RCLONE_DRY_RUN", "RESTIC_PASSWORD", "VP_OAUTH_CLIENT_SECRET"} {
		if strings.Contains(string(data), name+"=") {
			t.Fatalf("host setting inherited: %s", name)
		}
	}
	for _, value := range []string{"HTTPS_PROXY=http://p", "https_proxy=http://lower", "LC_ALL=C", "RCLONE_CONFIG=/x"} {
		if !strings.Contains(string(data), value) {
			t.Fatalf("allowed or explicit setting missing: %s", value)
		}
	}
}

func TestOutputLimitedAcceptsNormalAndExactLimitOutput(t *testing.T) {
	data, err := OutputLimited(context.Background(), os.Args[0], []string{"-test.run=TestProcessHelper"}, []string{"VP_PROCESS_HELPER=1", "VP_HELPER_MODE=output"}, 16)
	if err != nil || len(data) != 16 || !strings.Contains(string(data), "out-data") || !strings.Contains(string(data), "err-data") {
		t.Fatalf("normal combined output: %q %v", data, err)
	}
	data, err = OutputLimited(context.Background(), os.Args[0], []string{"-test.run=TestProcessHelper"}, []string{"VP_PROCESS_HELPER=1", "VP_HELPER_MODE=exact-output"}, 64)
	if err != nil || string(data) != strings.Repeat("x", 64) {
		t.Fatalf("exact limit output: %q %v", data, err)
	}
}

func TestOutputLimitedCancelsOnTotalStdoutAndStderrOverflow(t *testing.T) {
	for _, mode := range []string{"too-long", "stderr-too-long", "output"} {
		t.Run(mode, func(t *testing.T) {
			start := time.Now()
			data, err := OutputLimited(context.Background(), os.Args[0], []string{"-test.run=TestProcessHelper"}, []string{"VP_PROCESS_HELPER=1", "VP_HELPER_MODE=" + mode}, 12)
			if !errors.Is(err, ErrOutputLimitExceeded) || len(data) > 12 {
				t.Fatalf("overflow not bounded: %d bytes %v", len(data), err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("overflow did not stop promptly")
			}
		})
	}
}

func TestOutputLimitedRespectsContextAndExitCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := OutputLimited(ctx, os.Args[0], []string{"-test.run=TestProcessHelper"}, []string{"VP_PROCESS_HELPER=1", "VP_HELPER_MODE=wait"}, 64)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("context did not stop promptly: %v", err)
	}
	_, err = OutputLimited(context.Background(), os.Args[0], []string{"-test.run=TestProcessHelper"}, []string{"VP_PROCESS_HELPER=1", "VP_HELPER_MODE=exit3"}, 64)
	if ExitCode(err) != 3 {
		t.Fatalf("exit code lost: %v", err)
	}
	_, err = OutputLimited(context.Background(), os.Args[0], []string{"-test.run=TestProcessHelper"}, nil, 0)
	if err == nil {
		t.Fatal("zero output limit accepted")
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	_, err = OutputLimited(canceled, "nonexistent-binary", nil, nil, 64)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled context lost: %v", err)
	}
}
func TestExitCodeThreeIsNotSuccess(t *testing.T) {
	_, err := Output(context.Background(), os.Args[0], []string{"-test.run=TestProcessHelper"}, []string{"VP_PROCESS_HELPER=1", "VP_HELPER_MODE=exit3"})
	if ExitCode(err) != 3 {
		t.Fatal(err)
	}
}
func TestContextCancellationStopsProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Output(ctx, os.Args[0], []string{"-test.run=TestProcessHelper"}, []string{"VP_PROCESS_HELPER=1", "VP_HELPER_MODE=wait"})
	if err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancellation did not stop promptly")
	}
}
