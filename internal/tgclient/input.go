package tgclient

import (
	"context"
	"io"
)

// readInput allows a prompt to stop even when its reader cannot be interrupted.
// On cancellation at most one read remains until input arrives or the CLI exits;
// callers must not start another prompt on that reader afterwards.
func readInput(ctx context.Context, read func() ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := read()
		done <- result{data: data, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		return r.data, r.err
	}
}

type contextReader struct {
	ctx context.Context
	in  io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	data, err := readInput(r.ctx, func() ([]byte, error) {
		// The read can outlive Read on cancellation; it cannot borrow p.
		buf := make([]byte, len(p))
		n, err := r.in.Read(buf)
		return buf[:n], err //nolint:wrapcheck // Preserve io.Reader errors, including an exact io.EOF.
	})
	return copy(p, data), err
}
