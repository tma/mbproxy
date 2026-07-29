package modbus

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

type stubAddr string

func (a stubAddr) Network() string { return string(a) }
func (a stubAddr) String() string  { return string(a) }

type deadlineSpyConn struct {
	mu           sync.Mutex
	writes       int
	lastDeadline time.Time
}

func (c *deadlineSpyConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c *deadlineSpyConn) Write(data []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return len(data), nil
}

func (c *deadlineSpyConn) Close() error                     { return nil }
func (c *deadlineSpyConn) LocalAddr() net.Addr              { return stubAddr("local") }
func (c *deadlineSpyConn) RemoteAddr() net.Addr             { return stubAddr("remote") }
func (c *deadlineSpyConn) SetReadDeadline(time.Time) error  { return nil }
func (c *deadlineSpyConn) SetWriteDeadline(time.Time) error { return nil }

func (c *deadlineSpyConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.lastDeadline = deadline
	c.mu.Unlock()
	return nil
}

func (c *deadlineSpyConn) state() (int, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes, c.lastDeadline
}

func TestGuardedConn_CanceledRequestCannotWrite(t *testing.T) {
	tests := []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
		wantErr error
	}{
		{
			name: "canceled",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			wantErr: context.Canceled,
		},
		{
			name: "deadline expired",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			wantErr: context.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := &deadlineSpyConn{}
			conn := newGuardedConn(raw)
			ctx, cancel := tt.context()
			defer cancel()
			if err := conn.bind(ctx, time.Now().Add(time.Second)); err != nil {
				t.Fatalf("bind: %v", err)
			}
			if err := conn.SetDeadline(time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("grid-x deadline: %v", err)
			}

			n, err := conn.Write([]byte("request"))
			if n != 0 || !errors.Is(err, tt.wantErr) {
				t.Fatalf("write = (%d, %v), want (0, %v)", n, err, tt.wantErr)
			}
			writes, deadline := raw.state()
			if writes != 0 {
				t.Fatalf("raw writes = %d, want 0", writes)
			}
			if !deadline.Before(time.Now()) {
				t.Fatalf("canceled deadline was extended to %v", deadline)
			}
		})
	}
}

func TestGuardedConn_AbsoluteDeadlineCannotBeExtended(t *testing.T) {
	raw := &deadlineSpyConn{}
	conn := newGuardedConn(raw)
	maxDeadline := time.Now().Add(time.Minute)
	if err := conn.bind(context.Background(), maxDeadline); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	_, deadline := raw.state()
	if !deadline.Equal(maxDeadline) {
		t.Fatalf("raw deadline = %v, want cap %v", deadline, maxDeadline)
	}
}

func TestGuardedConn_NormalWrite(t *testing.T) {
	raw := &deadlineSpyConn{}
	conn := newGuardedConn(raw)
	if err := conn.bind(context.Background(), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("bind: %v", err)
	}

	n, err := conn.Write([]byte("request"))
	if err != nil || n != len("request") {
		t.Fatalf("write = (%d, %v)", n, err)
	}
	writes, _ := raw.state()
	if writes != 1 {
		t.Fatalf("raw writes = %d, want 1", writes)
	}
}

func TestGuardedConn_CancellationInterruptsActiveWrite(t *testing.T) {
	raw, peer := net.Pipe()
	defer raw.Close()
	defer peer.Close()
	conn := newGuardedConn(raw)
	if err := conn.bind(context.Background(), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("bind: %v", err)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("blocked request"))
		writeDone <- err
	}()

	deadline := time.Now().Add(time.Second)
	for {
		conn.mu.Lock()
		started := conn.writeStarted
		conn.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("write did not start")
		}
		runtime.Gosched()
	}

	if err := conn.cancel(context.Canceled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("active write was not interrupted")
		}
	case <-time.After(time.Second):
		t.Fatal("active write remained blocked after cancellation")
	}
}

func TestGuardedConn_LoopbackCanceledRequestSendsNoBytes(t *testing.T) {
	tests := []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
		wantErr error
	}{
		{
			name: "canceled",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			wantErr: context.Canceled,
		},
		{
			name: "deadline expired",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			wantErr: context.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer listener.Close()

			type readResult struct {
				n   int
				err error
			}
			serverRead := make(chan readResult, 1)
			go func() {
				server, acceptErr := listener.Accept()
				if acceptErr != nil {
					serverRead <- readResult{err: acceptErr}
					return
				}
				defer server.Close()
				if deadlineErr := server.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); deadlineErr != nil {
					serverRead <- readResult{err: deadlineErr}
					return
				}
				var data [1]byte
				n, readErr := server.Read(data[:])
				serverRead <- readResult{n: n, err: readErr}
			}()

			raw, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer raw.Close()
			conn := newGuardedConn(raw)
			ctx, cancel := tt.context()
			defer cancel()
			if err := conn.bind(ctx, time.Now().Add(time.Second)); err != nil {
				t.Fatalf("bind: %v", err)
			}
			if err := conn.SetDeadline(time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("grid-x deadline: %v", err)
			}

			n, err := conn.Write([]byte("request"))
			if n != 0 || !errors.Is(err, tt.wantErr) {
				t.Fatalf("write = (%d, %v), want (0, %v)", n, err, tt.wantErr)
			}
			select {
			case result := <-serverRead:
				if result.n != 0 {
					t.Fatalf("server received %d bytes", result.n)
				}
				if result.err == nil {
					t.Fatal("server read unexpectedly succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("server read did not complete")
			}
		})
	}
}
