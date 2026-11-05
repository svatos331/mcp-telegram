//go:build firestoreintegration

// Package firestoretest owns a Docker-backed emulator for one integration test binary.
package firestoretest

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	hostEnv = "FIRESTORE_EMULATOR_HOST"
	// Pin the multi-platform index so arm64 developers and amd64 CI use the same release.
	image = "gcr.io/google.com/cloudsdktool/google-cloud-cli:590.0.0-emulators@sha256:ba2726407ae3654d11ffdcd2eff74879f46c2ce805a8caa554d690afb615e433"
)

type emulator struct {
	host  string
	close func(context.Context) error
}

// Run belongs in TestMain. An explicit endpoint stays owned by its caller;
// otherwise the test binary starts and cleans up its own emulator container.
func Run(tests func() int) int { return run(tests, start) }

func run(tests func() int, launch func(context.Context) (emulator, error)) (result int) {
	if os.Getenv(hostEnv) != "" {
		return tests()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	owned, err := launch(ctx)
	cancel()
	if owned.close != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := owned.close(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "Firestore emulator cleanup:", err)
				if result == 0 {
					result = 1
				}
			}
		}()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Firestore emulator startup:", err)
		return 1
	}
	if owned.host == "" {
		fmt.Fprintln(os.Stderr, "Firestore emulator startup returned no endpoint")
		return 1
	}
	previous, wasSet := os.LookupEnv(hostEnv)
	if err := os.Setenv(hostEnv, owned.host); err != nil {
		fmt.Fprintln(os.Stderr, "setting Firestore test endpoint:", err)
		return 1
	}
	defer func() {
		var err error
		if wasSet {
			err = os.Setenv(hostEnv, previous)
		} else {
			err = os.Unsetenv(hostEnv)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "restoring Firestore test endpoint:", err)
			if result == 0 {
				result = 1
			}
		}
	}()
	return tests()
}

func start(ctx context.Context) (emulator, error) {
	container, err := testcontainers.Run(ctx, image,
		testcontainers.WithExposedPorts("8080/tcp"),
		testcontainers.WithCmd("gcloud", "emulators", "firestore", "start", "--host-port=0.0.0.0:8080", "--project=demo-mcp-telegram"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/").WithPort("8080/tcp").WithStartupTimeout(time.Minute)),
	)
	owned := emulator{}
	if container != nil {
		owned.close = func(ctx context.Context) error {
			if err := container.Terminate(ctx); err != nil {
				return fmt.Errorf("terminating Firestore container: %w", err)
			}
			return nil
		}
	}
	if err != nil {
		return owned, fmt.Errorf("starting Firestore container: %w", err)
	}
	owned.host, err = container.PortEndpoint(ctx, "8080/tcp", "")
	if err != nil {
		return owned, fmt.Errorf("resolving Firestore endpoint: %w", err)
	}
	return owned, nil
}
