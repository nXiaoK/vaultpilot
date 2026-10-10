package process

import (
	"context"
	"os"
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
	}
	os.Exit(0)
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
