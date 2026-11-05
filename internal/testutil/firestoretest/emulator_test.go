//go:build firestoreintegration

package firestoretest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunReusesSuppliedEmulator(t *testing.T) {
	t.Setenv("FIRESTORE_EMULATOR_HOST", "127.0.0.1:12345")
	code := run(func() int {
		assert.Equal(t, "127.0.0.1:12345", os.Getenv("FIRESTORE_EMULATOR_HOST"))
		return 7
	}, func(context.Context) (emulator, error) {
		t.Fatal("must not start a container for a supplied endpoint")
		return emulator{}, nil
	})
	assert.Equal(t, 7, code)
	assert.Equal(t, "127.0.0.1:12345", os.Getenv("FIRESTORE_EMULATOR_HOST"))
}

func TestRunOwnsContainerAndPreservesResult(t *testing.T) {
	for _, result := range []int{0, 7} {
		for _, cleanupFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("result=%d/cleanup-error=%t", result, cleanupFails), func(t *testing.T) {
				t.Setenv("FIRESTORE_EMULATOR_HOST", "")
				cleaned := false
				var startup context.Context
				code := run(func() int {
					assert.Equal(t, "127.0.0.1:12345", os.Getenv("FIRESTORE_EMULATOR_HOST"))
					assert.False(t, cleaned, "cleanup must follow the tests")
					return result
				}, func(ctx context.Context) (emulator, error) {
					startup = ctx
					_, bounded := ctx.Deadline()
					assert.True(t, bounded, "startup must be bounded")
					return emulator{host: "127.0.0.1:12345", close: func(ctx context.Context) error {
						assert.ErrorIs(t, startup.Err(), context.Canceled)
						assert.NoError(t, ctx.Err(), "cleanup must not inherit a cancelled startup context")
						_, bounded := ctx.Deadline()
						assert.True(t, bounded)
						cleaned = true
						if cleanupFails {
							return errors.New("injected cleanup error")
						}
						return nil
					}}, nil
				})
				expected := result
				if cleanupFails && expected == 0 {
					expected = 1
				}
				assert.Equal(t, expected, code)
				assert.True(t, cleaned)
				assert.Empty(t, os.Getenv("FIRESTORE_EMULATOR_HOST"))
			})
		}
	}
}

func TestRunCleansPartialStartup(t *testing.T) {
	t.Setenv("FIRESTORE_EMULATOR_HOST", "")
	cleaned := false
	code := run(func() int { t.Fatal("tests must not run after startup failure"); return 0 }, func(context.Context) (emulator, error) {
		return emulator{close: func(ctx context.Context) error { require.NoError(t, ctx.Err()); cleaned = true; return nil }}, errors.New("injected startup error")
	})
	assert.Equal(t, 1, code)
	assert.True(t, cleaned)
	assert.Empty(t, os.Getenv("FIRESTORE_EMULATOR_HOST"))
}

func TestRunCleansUpOnPanic(t *testing.T) {
	t.Setenv("FIRESTORE_EMULATOR_HOST", "")
	cleaned := false
	require.Panics(t, func() {
		run(func() int { panic("test runner panicked") }, func(context.Context) (emulator, error) {
			return emulator{host: "127.0.0.1:12345", close: func(context.Context) error { cleaned = true; return nil }}, nil
		})
	})
	assert.True(t, cleaned)
	assert.Empty(t, os.Getenv("FIRESTORE_EMULATOR_HOST"))
}
