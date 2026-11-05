package sessionstore

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// Count logical writes, not RPC attempts: retries within the SDK must not be
// mistaken for another client's write and re-enter the grant replay rules.
type grantWriteCounter struct {
	backend
	stores int
}

func (b *grantWriteCounter) StoreGrant(ctx context.Context, family string, grant GrantRecord, version int64) error {
	b.stores++
	return b.backend.StoreGrant(ctx, family, grant, version)
}

func TestFirestoreGrantSurvivesRPCRetries(t *testing.T) {
	host := os.Getenv("FIRESTORE_EMULATOR_HOST")
	if host == "" {
		t.Skip("requires Firestore emulator")
	}
	for _, fault := range []string{"aborted before commit", "response lost after commit"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var injected atomic.Bool
			var begins atomic.Int64
			interceptor := func(ctx context.Context, method string, request, response any, conn *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
				if strings.HasSuffix(method, "/BeginTransaction") {
					begins.Add(1)
				}
				if strings.HasSuffix(method, "/Commit") && injected.CompareAndSwap(false, true) {
					if fault == "aborted before commit" {
						return status.Error(codes.Aborted, "injected transaction conflict")
					}
					if err := invoke(ctx, method, request, response, conn, opts...); err != nil {
						return err
					}
					// The emulator has committed. Only its acknowledgement is lost, so the
					// real SDK's Commit and transaction retry policies now decide the path.
					return status.Error(codes.Unavailable, "injected lost commit response")
				}
				return invoke(ctx, method, request, response, conn, opts...)
			}
			conn, err := grpc.NewClient(host, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithChainUnaryInterceptor(interceptor))
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			// The SDK emulator shortcut builds its own connection, bypassing custom
			// dial options. Supply our emulator-only connection without ADC instead.
			t.Setenv("FIRESTORE_EMULATOR_HOST", "")
			client, err := firestore.NewClient(ctx, "demo-retry-"+NewSID(), option.WithGRPCConn(conn), option.WithoutAuthentication())
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			b := &grantWriteCounter{backend: &Firestore{client: client}}
			store := Encrypted(b, newCipher(t, testIssuer, newKey(t)))
			family := NewSID()
			now := time.Now()
			expiry := now.Add(time.Hour).UTC()

			created, err := store.RedeemCode(ctx, family, expiry)
			require.NoError(t, err)
			require.True(t, created, "our committed code redemption must not be classified as code reuse")
			require.True(t, injected.Load(), "the RPC fault must actually have fired")
			assert.Equal(t, 1, b.stores, "SDK retry must stay within one logical backend write")
			if fault == "aborted before commit" {
				assert.GreaterOrEqual(t, begins.Load(), int64(2))
			}

			injected.Store(false)
			beforeBegins := begins.Load()
			result, err := store.RotateGrant(ctx, family, 0, now)
			require.NoError(t, err)
			assert.Equal(t, GrantRotated, result, "our landed refresh write must not revoke the family")
			assert.Equal(t, 2, b.stores)
			require.True(t, injected.Load())
			if fault == "aborted before commit" {
				assert.GreaterOrEqual(t, begins.Load()-beforeBegins, int64(2))
			}
			grant, _, err := b.LoadGrant(ctx, family)
			require.NoError(t, err)
			assert.Equal(t, int64(1), grant.Generation)
			assert.Equal(t, expiry, grant.ExpiresAt)
			assert.False(t, grant.Revoked)
			assert.NotEmpty(t, grant.WriteID)

			result, err = store.RotateGrant(ctx, family, 0, now)
			require.NoError(t, err)
			assert.Equal(t, GrantReplay, result, "a separate genuine stale-generation request must still revoke")
			grant, _, err = b.LoadGrant(ctx, family)
			require.NoError(t, err)
			assert.True(t, grant.Revoked)

			revocationFamily := NewSID()
			created, err = store.RedeemCode(ctx, revocationFamily, expiry)
			require.NoError(t, err)
			require.True(t, created)
			injected.Store(false)
			beforeStores := b.stores
			require.NoError(t, store.RevokeGrant(ctx, revocationFamily))
			assert.Equal(t, 1, b.stores-beforeStores, "our landed revocation must not be repeated as a foreign conflict")
			require.True(t, injected.Load())
			grant, _, err = b.LoadGrant(ctx, revocationFamily)
			require.NoError(t, err)
			assert.True(t, grant.Revoked)
		})
	}
}
