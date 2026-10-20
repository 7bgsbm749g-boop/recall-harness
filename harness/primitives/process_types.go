package primitives

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

const (
	PrimitiveEventProcessStarted      PrimitiveEventType = "process.started"
	PrimitiveEventProcessOutput       PrimitiveEventType = "process.output"
	PrimitiveEventProcessStreamFailed PrimitiveEventType = "process.stream_failed"
	PrimitiveEventProcessExited       PrimitiveEventType = "process.exited"

	PrimitiveEventProcessInputWritten     PrimitiveEventType = "process.input_written"
	PrimitiveEventProcessInputWriteFailed PrimitiveEventType = "process.input_write_failed"
	PrimitiveEventProcessInputClosed      PrimitiveEventType = "process.input_closed"
	PrimitiveEventProcessSignaled         PrimitiveEventType = "process.signaled"

	ProcessOutputChunkSize = 32 * 1024

	processTerminationGracePeriod = 5 * time.Second
)

var errProcessDone = errors.New("process invocation is done")

type ProcessStartRequest struct {
	Source        SourceID
	CorrelationID CorrelationID
	// Path must be absolute.
	Path        string
	Arguments   []string
	Directory   string
	Environment []string
	Pipes       ProcessPipeSet
	// Output capture paths must be absolute, name existing regular files, and
	// are truncated before start. Each is mutually exclusive with its pipe.
	StdoutPath string
	StderrPath string
}

type ProcessPipeSet uint8

const (
	ProcessPipeStdin ProcessPipeSet = 1 << iota
	ProcessPipeStdout
	ProcessPipeStderr

	ProcessPipeAll = ProcessPipeStdin | ProcessPipeStdout | ProcessPipeStderr
)

type ProcessStartedResult struct {
	PID int
}

type ProcessStream int

const (
	ProcessStdout ProcessStream = 1
	ProcessStderr ProcessStream = 2
)

type ProcessOutputResult struct {
	Stream ProcessStream
	Offset int64
	Data   []byte
}

type ProcessStreamFailureResult struct {
	Stream ProcessStream
	Error  string
}

type ProcessExitResult struct {
	ExitCode int
	Signal   syscall.Signal
}

type ProcessWriteRequest struct {
	Source        SourceID
	CorrelationID CorrelationID
	Data          []byte
}

type ProcessWriteResult struct {
	Count int
}

type ProcessWriteFailureResult struct {
	Count int
	Error string
}

type ProcessCloseInputRequest struct {
	Source        SourceID
	CorrelationID CorrelationID
}

type ProcessSignalRequest struct {
	Source              SourceID
	CorrelationID       CorrelationID
	Signal              syscall.Signal
	PropagateToChildren bool
}

type ProcessInvocation struct {
	ctx      context.Context
	ready    chan struct{}
	process  *os.Process
	stdin    *os.File
	startErr error
}
