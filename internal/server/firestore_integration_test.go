//go:build firestoreintegration

package server

import (
	"os"
	"testing"

	"github.com/tolmachov/mcp-telegram/internal/testutil/firestoretest"
)

func TestMain(m *testing.M) {
	os.Exit(firestoretest.Run(m.Run))
}
