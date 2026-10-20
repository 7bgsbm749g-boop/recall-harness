//go:build unix

package acp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/recall/memory"
	"golang.org/x/sys/unix"
)

// T3/Bun supplies Unix sockets for stdio. Closing a Go os.File can itself
// wait for a blocking read, so io.Pipe tests alone do not prove shutdown.
func TestSIGTERMStopsACPWithParentStdinSocketOpen(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		unix.CloseOnExec(fd)
	}
	parent := os.NewFile(uintptr(fds[0]), "parent")
	defer parent.Close()
	child := os.NewFile(uintptr(fds[1]), "child")
	defer child.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestACPSocketHelperProcess$")
	command.Env = append(os.Environ(), "RECALL_ACP_SOCKET_HELPER=1", "RECALL_ACP_HELPER_DATA="+t.TempDir())
	command.Stdin = child
	command.Stderr = io.Discard
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	_ = child.Close()
	if err = json.NewEncoder(parent).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 2}}); err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID int `json:"id"`
	}
	if err = json.NewDecoder(stdout).Decode(&response); err != nil || response.ID != 1 {
		t.Fatal("helper initialization failed", err)
	}
	if err = command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ACP shutdown waited for the parent to close its stdin socket")
	}
}

func TestACPSocketHelperProcess(t *testing.T) {
	if os.Getenv("RECALL_ACP_SOCKET_HELPER") != "1" {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err := Serve(ctx, os.Stdin, os.Stdout, Config{DataDirectory: os.Getenv("RECALL_ACP_HELPER_DATA"), Model: "fixture", Memory: memory.DefaultConfig(), Client: fakeAdapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return assistant("unused"), nil
	})})
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
