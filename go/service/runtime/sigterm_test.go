package runtime

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/wandering-compiler/sdk/go/core/observx"
	"github.com/wandering-compiler/sdk/go/service/bootstrap"
)

// What a deploy does to a running binary: SIGTERM. The supervisor drains the
// gRPC component and the process exits with status 0 — a clean stop, not a
// failure the orchestrator would restart or alert on.
func TestGRPCComponent_SIGTERMExitsCleanly(t *testing.T) {
	if os.Getenv("RUNTIME_SIGTERM_CHILD") == "1" {
		addr := os.Getenv("RUNTIME_SIGTERM_ADDR")
		comp := GRPCComponent(Config{Addr: addr, RegisterServices: func(*grpc.Server) error {
			fmt.Println("registered")
			return nil
		}})
		if err := bootstrap.Run(context.Background(), observx.Config{ServiceName: "sigterm-child"}, comp); err != nil {
			fmt.Println("run error:", err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	addr := freeAddr(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestGRPCComponent_SIGTERMExitsCleanly$")
	cmd.Env = append(os.Environ(), "RUNTIME_SIGTERM_CHILD=1", "RUNTIME_SIGTERM_ADDR="+addr, "W17_INTERNAL_TLS=")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	ready := make(chan bool, 1)
	var lines []string
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines = append(lines, sc.Text())
			if sc.Text() == "registered" {
				ready <- true
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("the child ended before serving")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the child never served")
	}
	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("SIGTERM ended the process with %v (output %s), want status 0", err, strings.Join(lines, " | "))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the process did not stop after SIGTERM")
	}
}
