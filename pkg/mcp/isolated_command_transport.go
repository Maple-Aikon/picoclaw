package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sipeed/picoclaw/pkg/isolation"
)

var isolatedCommandTerminateDuration = 5 * time.Second

// isolatedCommandTransport mirrors the SDK command transport but routes
// process startup through pkg/isolation so Windows post-start hooks run too.
type isolatedCommandTransport struct {
	Command           *exec.Cmd
	TerminateDuration time.Duration

	// waitOnce guards the single cmd.Wait() call.
	//
	// Before 2026-11-16 two different places called cmd.Wait(): this file's
	// Close() and the Manager's process waiter. Go's os/exec allows only one
	// Wait per Cmd - the second call fails with "waitid: no child
	// processes", which surfaced as a Close() error.
	//
	// The transport owns the wait now: it starts one waiter goroutine at
	// Connect time and both Close() and the Manager observe its result.
	//
	// R10 (2026-11-16): the result is BROADCAST, not handed out.
	//
	// The previous shape was a buffered channel (capacity 1) that every
	// reader put the value back into. That shape WORKS - the channel
	// serialises the hand-off, so N readers take turns and none of them
	// deadlocks (measured: mutation 5 reverted the broadcast and R13
	// still passed, which is how the claim was corrected).
	//
	// The broadcast is kept because it is simpler and has no window at
	// all: the result is stored once and the channel is closed, so every
	// reader sees the same value without touching a shared slot.
	waitOnce sync.Once
	waitCh   chan struct{}
	// waitErr holds the process exit error. It is written by the waiter
	// goroutine before waitCh is closed and read by every waiter after
	// waitCh is closed, so the close/receive pair provides the
	// happens-before edge. It is still an atomic to be explicit about it.
	waitErr atomic.Value
	// unboundedWaitCount counts the waitResult calls made with a
	// non-positive timeout, i.e. "wait until the process exits".
	//
	// R12 (2026-11-16): it exists so a test can assert the CONTRACT -
	// "the Manager's process waiter waits for the exit, it does not give
	// up after a fixed budget" - without having to wait that budget in
	// real time. The live bug was exactly a 60s budget, and a 60s test is
	// too slow to run in a unit suite.
	unboundedWaitCount atomic.Int64
}

func (t *isolatedCommandTransport) startWaiter() {
	if t.waitCh != nil {
		return
	}
	t.waitCh = make(chan struct{})
	go func() {
		err := t.Command.Wait()
		t.waitErr.Store(&waitError{err: err})
		close(t.waitCh)
	}()
}

// waitError wraps the exit error so it can live in an atomic.Value
// (which rejects nil and requires a concrete type).
type waitError struct {
	err error
}

// waitResult waits for the process exit result, tolerating repeated calls.
//
// R10 (2026-11-16): a timeout of 0 means "wait forever". The previous
// behaviour - a fixed 60s timeout - was shorter than the idle timeout
// (10m) and the max lifetime (2h), so the waiter gave up while the
// process was still alive and the manager never learned that a reaped
// server had died.
//
// The result is broadcast: the channel is closed when the process exits
// and the value is stored in t.waitErr, so any number of readers can
// wait for it without blocking each other.
func (t *isolatedCommandTransport) waitResult(timeout time.Duration) (error, bool) {
	if t.waitCh == nil {
		t.startWaiter()
	}
	if timeout <= 0 {
		t.unboundedWaitCount.Add(1)
		<-t.waitCh
		return t.storedWaitErr(), true
	}
	select {
	case <-t.waitCh:
		return t.storedWaitErr(), true
	case <-time.After(timeout):
		return nil, false
	}
}

// storedWaitErr returns the exit error recorded by the waiter goroutine.
func (t *isolatedCommandTransport) storedWaitErr() error {
	v := t.waitErr.Load()
	if v == nil {
		return nil
	}
	w, ok := v.(*waitError)
	if !ok || w == nil {
		return nil
	}
	return w.err
}

func (t *isolatedCommandTransport) Connect(ctx context.Context) (sdkmcp.Connection, error) {
	stdout, err := t.Command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdout = io.NopCloser(stdout)
	stdin, err := t.Command.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := isolation.Start(t.Command); err != nil {
		return nil, err
	}
	td := t.TerminateDuration
	if td <= 0 {
		td = isolatedCommandTerminateDuration
	}
	// P1 (2026-11-16): start the single waiter goroutine here. From this
	// point on, cmd.Wait() is owned by the transport - neither Close() nor
	// the Manager may call it again.
	t.startWaiter()
	return newIsolatedIOConn(&isolatedPipeRWC{
		cmd:               t.Command,
		stdout:            stdout,
		stdin:             stdin,
		terminateDuration: td,
		transport:         t,
	}), nil
}

type isolatedPipeRWC struct {
	cmd               *exec.Cmd
	stdout            io.ReadCloser
	stdin             io.WriteCloser
	terminateDuration time.Duration
	transport         *isolatedCommandTransport
}

func (s *isolatedPipeRWC) Read(p []byte) (n int, err error) {
	return s.stdout.Read(p)
}

func (s *isolatedPipeRWC) Write(p []byte) (n int, err error) {
	return s.stdin.Write(p)
}

func (s *isolatedPipeRWC) Close() error {
	if err := s.stdin.Close(); err != nil {
		return fmt.Errorf("closing stdin: %w", err)
	}
	// The transport owns the single cmd.Wait() call (see
	// isolatedCommandTransport.startWaiter); this method must not call
	// cmd.Wait() again or Go reports "waitid: no child processes".
	resChan := make(chan error, 1)
	go func() {
		err, ok := s.transport.waitResult(s.terminateDuration)
		if !ok {
			err = fmt.Errorf("subprocess did not exit within %s", s.terminateDuration)
		}
		resChan <- err
	}()
	wait := func() (error, bool) {
		select {
		case err := <-resChan:
			return err, true
		case <-time.After(s.terminateDuration):
		}
		return nil, false
	}
	if err, ok := wait(); ok {
		return err
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err == nil {
		if err, ok := wait(); ok {
			return err
		}
	}
	if err := s.cmd.Process.Kill(); err != nil {
		return err
	}
	if err, ok := wait(); ok {
		return err
	}
	return fmt.Errorf("unresponsive subprocess")
}

type isolatedIOConn struct {
	writeMu   sync.Mutex
	rwc       io.ReadWriteCloser
	incoming  <-chan isolatedMsgOrErr
	queue     []jsonrpc.Message
	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

type isolatedMsgOrErr struct {
	msg json.RawMessage
	err error
}

func newIsolatedIOConn(rwc io.ReadWriteCloser) *isolatedIOConn {
	incoming := make(chan isolatedMsgOrErr)
	closed := make(chan struct{})
	go func() {
		dec := json.NewDecoder(rwc)
		for {
			var raw json.RawMessage
			err := dec.Decode(&raw)
			if err == nil {
				var tr [1]byte
				if n, readErr := dec.Buffered().Read(tr[:]); n > 0 {
					if tr[0] != '\n' && tr[0] != '\r' {
						err = fmt.Errorf("invalid trailing data at the end of stream")
					}
				} else if readErr != nil && readErr != io.EOF {
					err = readErr
				}
			}
			select {
			case incoming <- isolatedMsgOrErr{msg: raw, err: err}:
			case <-closed:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return &isolatedIOConn{rwc: rwc, incoming: incoming, closed: closed}
}

func (c *isolatedIOConn) SessionID() string { return "" }

func (c *isolatedIOConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if len(c.queue) > 0 {
		next := c.queue[0]
		c.queue = c.queue[1:]
		return next, nil
	}
	var raw json.RawMessage
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case v := <-c.incoming:
		if v.err != nil {
			return nil, v.err
		}
		raw = v.msg
	case <-c.closed:
		return nil, io.EOF
	}
	msgs, err := readIsolatedBatch(raw)
	if err != nil {
		return nil, err
	}
	c.queue = msgs[1:]
	return msgs[0], nil
}

func readIsolatedBatch(data []byte) ([]jsonrpc.Message, error) {
	var rawBatch []json.RawMessage
	if err := json.Unmarshal(data, &rawBatch); err == nil {
		if len(rawBatch) == 0 {
			return nil, fmt.Errorf("empty batch")
		}
		msgs := make([]jsonrpc.Message, 0, len(rawBatch))
		for _, raw := range rawBatch {
			msg, err := jsonrpc.DecodeMessage(raw)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, msg)
		}
		return msgs, nil
	}
	msg, err := jsonrpc.DecodeMessage(data)
	if err != nil {
		return nil, err
	}
	return []jsonrpc.Message{msg}, nil
}

func (c *isolatedIOConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}
	data = append(data, '\n')
	_, err = c.rwc.Write(data)
	return err
}

func (c *isolatedIOConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.rwc.Close()
		close(c.closed)
	})
	return c.closeErr
}

var (
	_ sdkmcp.Transport  = (*isolatedCommandTransport)(nil)
	_ sdkmcp.Connection = (*isolatedIOConn)(nil)
)
