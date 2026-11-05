package internal

import (
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
)

// Observe real client connections without adding a production factory seam.
// The root context stays live: only the run command's cleanup can release them.
func TestRunClosesFirestoreOnStartupFailure(t *testing.T) {
	emulator := os.Getenv("FIRESTORE_EMULATOR_HOST")
	if emulator == "" {
		t.Skip("requires Firestore emulator")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var opened, active atomic.Int64
	var connections sync.Map
	t.Cleanup(func() {
		_ = listener.Close()
		connections.Range(func(key, value any) bool { _ = key.(net.Conn).Close(); return true })
	})
	go func() {
		for {
			downstream, err := listener.Accept()
			if err != nil {
				return
			}
			opened.Add(1)
			active.Add(1)
			connections.Store(downstream, struct{}{})
			go func() {
				defer active.Add(-1)
				defer connections.Delete(downstream)
				defer func() { _ = downstream.Close() }()
				upstream, err := net.DialTimeout("tcp", emulator, time.Second) //nolint:gosec // G704: test-only proxy to the explicitly configured emulator.
				if err != nil {
					return
				}
				defer func() { _ = upstream.Close() }()
				copied := make(chan struct{})
				go func() { _, _ = io.Copy(upstream, downstream); _ = upstream.Close(); close(copied) }()
				_, _ = io.Copy(downstream, upstream)
				_ = downstream.Close()
				<-copied
			}()
		}
	}()
	t.Setenv("FIRESTORE_EMULATOR_HOST", listener.Addr().String())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cmd := New(strings.NewReader(""), io.Discard, io.Discard)
	err = cmd.Run(t.Context(), []string{"mcp-telegram", "run",
		"--transport", "http", "--http-addr", "127.0.0.1:8080",
		"--api-id", "1", "--api-hash", "test-hash",
		"--auth-issuer-url", "http://127.0.0.1:8080", "--auth-allowed-users", "42",
		"--auth-token-key", testKey(t), "--auth-firestore-project", "demo-cleanup-" + sessionstore.NewSID(),
		"--auth-firestore-database", "(default)", "--variant", "not-a-variant",
	})
	require.ErrorContains(t, err, "unknown variant")
	require.Positive(t, opened.Load(), "the storage constructor must have completed before startup failed")
	require.Eventually(t, func() bool { return active.Load() == 0 }, 5*time.Second, 10*time.Millisecond, "startup failure must not leak a Firestore connection")
}
