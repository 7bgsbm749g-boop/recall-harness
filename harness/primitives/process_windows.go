package primitives

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Windows commands use a captured child PID for tree cancellation. Recall's
// model-visible command tool also applies this rule in its ACP operation path.
func StartProcess(ctx context.Context, request ProcessStartRequest, events chan<- PrimitiveEvent) *ProcessInvocation {
	p := &ProcessInvocation{ctx: ctx, ready: make(chan struct{})}
	go func() {
		fail := func(err error) {
			p.startErr = err
			close(p.ready)
			events <- primitiveFailure(request.Source, request.CorrelationID, err)
		}
		if !filepath.IsAbs(request.Path) {
			fail(errors.New("process path must be absolute"))
			return
		}
		command := exec.CommandContext(ctx, request.Path, request.Arguments...)
		command.Dir = request.Directory
		command.Env = request.Environment
		command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		command.WaitDelay = 2 * time.Second
		command.Cancel = func() error {
			if command.Process == nil {
				return nil
			}
			return exec.Command("taskkill.exe", "/PID", fmt.Sprint(command.Process.Pid), "/T", "/F").Run()
		}
		var outputs []struct {
			stream ProcessStream
			reader io.ReadCloser
		}
		var captures []*os.File
		defer func() {
			for _, f := range captures {
				_ = f.Close()
			}
		}()
		if request.Pipes&ProcessPipeStdin != 0 {
			pipe, err := command.StdinPipe()
			if err != nil {
				fail(err)
				return
			}
			p.stdin = pipe.(*os.File)
		}
		for _, output := range []struct {
			stream ProcessStream
			flag   ProcessPipeSet
			path   string
		}{{ProcessStdout, ProcessPipeStdout, request.StdoutPath}, {ProcessStderr, ProcessPipeStderr, request.StderrPath}} {
			if output.path != "" {
				if request.Pipes&output.flag != 0 || !filepath.IsAbs(output.path) {
					fail(errors.New("invalid process capture configuration"))
					return
				}
				info, err := os.Lstat(output.path)
				if err != nil || !info.Mode().IsRegular() {
					fail(errors.New("capture must be an existing regular file"))
					return
				}
				file, err := os.OpenFile(output.path, os.O_WRONLY|os.O_TRUNC, 0600)
				if err != nil {
					fail(err)
					return
				}
				captures = append(captures, file)
				if output.stream == ProcessStdout {
					command.Stdout = file
				} else {
					command.Stderr = file
				}
			} else if request.Pipes&output.flag != 0 {
				var reader io.ReadCloser
				var err error
				if output.stream == ProcessStdout {
					reader, err = command.StdoutPipe()
				} else {
					reader, err = command.StderrPipe()
				}
				if err != nil {
					fail(err)
					return
				}
				outputs = append(outputs, struct {
					stream ProcessStream
					reader io.ReadCloser
				}{output.stream, reader})
			}
		}
		if err := command.Start(); err != nil {
			fail(err)
			return
		}
		p.process = command.Process
		close(p.ready)
		select {
		case events <- PrimitiveEvent{Type: PrimitiveEventProcessStarted, Source: request.Source, CorrelationID: request.CorrelationID, Result: ProcessStartedResult{PID: command.Process.Pid}}:
		case <-ctx.Done():
		}
		var readers sync.WaitGroup
		for _, output := range outputs {
			readers.Go(func() {
				defer output.reader.Close()
				buffer := make([]byte, ProcessOutputChunkSize)
				var offset int64
				for {
					n, err := output.reader.Read(buffer)
					if n > 0 {
						event := PrimitiveEvent{Type: PrimitiveEventProcessOutput, Source: request.Source, CorrelationID: request.CorrelationID, Result: ProcessOutputResult{Stream: output.stream, Offset: offset, Data: append([]byte(nil), buffer[:n]...)}}
						select {
						case events <- event:
						case <-ctx.Done():
						}
						offset += int64(n)
					}
					if err != nil {
						return
					}
				}
			})
		}
		err := command.Wait()
		readers.Wait()
		for _, f := range captures {
			_ = f.Sync()
		}
		if ctx.Err() != nil {
			events <- primitiveCanceled(request.Source, request.CorrelationID)
			return
		}
		if command.ProcessState == nil {
			events <- primitiveFailure(request.Source, request.CorrelationID, err)
			return
		}
		events <- PrimitiveEvent{Type: PrimitiveEventProcessExited, Source: request.Source, CorrelationID: request.CorrelationID, Result: ProcessExitResult{ExitCode: command.ProcessState.ExitCode()}}
	}()
	return p
}

func (p *ProcessInvocation) readyFor(ctx context.Context) error {
	select {
	case <-p.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.startErr != nil {
		return p.startErr
	}
	if p.ctx.Err() != nil {
		return errProcessDone
	}
	return nil
}
func (p *ProcessInvocation) WriteInput(ctx context.Context, request ProcessWriteRequest, events chan<- PrimitiveEvent) {
	go func() {
		if err := p.readyFor(ctx); err != nil {
			events <- primitiveFailure(request.Source, request.CorrelationID, err)
			return
		}
		if p.stdin == nil {
			events <- primitiveFailure(request.Source, request.CorrelationID, errors.New("stdin is unavailable"))
			return
		}
		n, err := p.stdin.Write(request.Data)
		if err != nil {
			events <- PrimitiveEvent{Type: PrimitiveEventProcessInputWriteFailed, Source: request.Source, CorrelationID: request.CorrelationID, Result: ProcessWriteFailureResult{Count: n, Error: err.Error()}}
			return
		}
		events <- PrimitiveEvent{Type: PrimitiveEventProcessInputWritten, Source: request.Source, CorrelationID: request.CorrelationID, Result: ProcessWriteResult{Count: n}}
	}()
}
func (p *ProcessInvocation) CloseInput(ctx context.Context, request ProcessCloseInputRequest, events chan<- PrimitiveEvent) {
	go func() {
		if err := p.readyFor(ctx); err != nil {
			events <- primitiveFailure(request.Source, request.CorrelationID, err)
			return
		}
		if p.stdin != nil {
			if err := p.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				events <- primitiveFailure(request.Source, request.CorrelationID, err)
				return
			}
		}
		events <- PrimitiveEvent{Type: PrimitiveEventProcessInputClosed, Source: request.Source, CorrelationID: request.CorrelationID}
	}()
}
func (p *ProcessInvocation) Signal(ctx context.Context, request ProcessSignalRequest, events chan<- PrimitiveEvent) {
	go func() {
		if err := p.readyFor(ctx); err != nil {
			events <- primitiveFailure(request.Source, request.CorrelationID, err)
			return
		}
		var err error
		if request.PropagateToChildren {
			err = exec.CommandContext(ctx, "taskkill.exe", "/PID", fmt.Sprint(p.process.Pid), "/T", "/F").Run()
		} else {
			err = p.process.Kill()
		}
		if err != nil {
			events <- primitiveFailure(request.Source, request.CorrelationID, err)
			return
		}
		events <- PrimitiveEvent{Type: PrimitiveEventProcessSignaled, Source: request.Source, CorrelationID: request.CorrelationID}
	}()
}
