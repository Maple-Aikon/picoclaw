package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
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
	waitOnce sync.Once
	waitCh   chan error
}

func (t *isolatedCommandTransport) startWaiter() {
	if t.waitCh != nil {
		return
	}
	t.waitCh = make(chan error, 1)
	go func() {
		t.waitCh <- t.Command.Wait()
	}()
}

// waitResult waits for the process exit result, tolerating repeated calls.
func (t *isolatedCommandTransport) waitResult(timeout time.Duration) (error, bool) {
	if t.waitCh == nil {
		t.startWaiter()
	}
	select {
	case err := <-t.waitCh:
		// Put it back so later callers see the same result.
		t.waitCh <- err
		return err, true
	case <-time.After(timeout):
		return nil, false
	}
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
