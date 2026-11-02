package tgclient

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextReaderCancellationDoesNotBorrowCallerBuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	in := inputReaderFunc(func(p []byte) (int, error) {
		close(started)
		<-release
		p[0] = 'x'
		close(finished)
		return 1, nil
	})
	buf := []byte("original")
	done := make(chan error, 1)
	go func() {
		_, err := (contextReader{ctx: ctx, in: in}).Read(buf)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		close(release)
		t.Fatal("read did not stop on cancellation")
	}
	close(release)
	<-finished
	assert.Equal(t, "original", string(buf), "a cancelled read must not later overwrite the caller's buffer")
}

func TestContextReaderAlreadyCancelledDoesNotRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	in := inputReaderFunc(func([]byte) (int, error) {
		t.Error("input read after cancellation")
		return 0, io.EOF
	})
	_, err := (contextReader{ctx: ctx, in: in}).Read(make([]byte, 1))
	require.ErrorIs(t, err, context.Canceled)
}

type inputReaderFunc func([]byte) (int, error)

func (f inputReaderFunc) Read(p []byte) (int, error) { return f(p) }
