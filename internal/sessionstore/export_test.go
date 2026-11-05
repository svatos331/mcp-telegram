package sessionstore

import (
	"os"
	"testing"

	"cloud.google.com/go/firestore"
	"github.com/stretchr/testify/require"
)

// NewTestFirestore uses only the explicitly configured emulator, never ADC or
// a real project. Each test gets its own project namespace.
func NewTestFirestore(t *testing.T) *Firestore {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("run with -tags=firestoreintegration or set FIRESTORE_EMULATOR_HOST")
	}
	client, err := firestore.NewClient(t.Context(), "demo-mcp-"+NewSID())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return &Firestore{client: client}
}
