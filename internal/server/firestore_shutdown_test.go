package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
)

// Use the production constructor: the ordinary emulator helper bypasses
// resource ownership and cannot catch a client closed at the start of drain.
func TestFirestoreRemainsUsableDuringHTTPDrain(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("requires Firestore emulator")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store, err := sessionstore.NewFirestore(ctx, "demo-drain-"+sessionstore.NewSID(), "(default)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	sid := sessionstore.NewSID()
	require.NoError(t, store.Session(42, sid).StoreSession(t.Context(), []byte("before shutdown")))
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	requestErr := make(chan error, 1)
	storageErr := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		close(entered)
		<-release
		requestErr <- r.Context().Err()
		err := store.Session(42, sid).StoreSession(r.Context(), []byte("during drain"))
		storageErr <- err
		if err != nil {
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	addr := freePort(t)
	url := fmt.Sprintf("http://%s", addr)
	serverDone := make(chan error, 1)
	go func() { serverDone <- testServer(t).serveHTTP(ctx, handler, addr, 3*time.Second) }()
	_ = waitForServer(t, url+"/ready").Body.Close()
	response := make(chan int, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/accepted", nil)
		if err != nil {
			response <- 0
			return
		}
		resp, err := testClient.Do(req)
		if err != nil {
			response <- 0
			return
		}
		_ = resp.Body.Close()
		response <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request was not accepted")
	}
	cancel()
	// Hold the accepted request inside the drain window. A root-cancellation
	// cleanup must not make the shared client unavailable while it is pending.
	var probeErr error
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, probeErr = store.Exists(t.Context(), 42, sid); probeErr != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	unblock()
	require.NoError(t, <-requestErr, "the accepted request must retain its live context")
	writeErr := <-storageErr
	status := <-response
	require.NoError(t, <-serverDone)
	require.NoError(t, probeErr, "Firestore must remain available during graceful drain")
	require.NoError(t, writeErr, "an accepted request must finish its durable write")
	require.Equal(t, http.StatusOK, status)
}
