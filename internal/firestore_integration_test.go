//go:build firestoreintegration

package internal

import (
	"os"
	"testing"

	"github.com/tolmachov/mcp-telegram/internal/testutil/firestoretest"
)

func TestMain(m *testing.M) {
	os.Exit(firestoretest.Run(m.Run))
}
