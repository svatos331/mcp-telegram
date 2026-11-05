package sessionstore

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirestoreImportedSession(t *testing.T) {
	b := NewTestFirestore(t)
	ctx := t.Context()
	cipher := newCipher(t, testIssuer, newKey(t))
	key := userKeyForTest(t)
	blob, err := sessionCipherFor(t, cipher, 42, key).seal([]byte("mtproto"))
	require.NoError(t, err)
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err = b.client.Collection(firestoreSessions).Doc(sessionBase(42, testSID)).Set(ctx, map[string]any{"data": blob, "updated_at": updated})
	require.NoError(t, err)
	refs, err := b.List(ctx)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, updated, refs[0].UpdatedAt, "migration must preserve orphan-sweep age")
	got, err := Encrypted(b, cipher).Session(42, testSID, key).LoadSession(ctx)
	require.NoError(t, err)
	assert.Equal(t, []byte("mtproto"), got)
	raw, err := b.Session(42, testSID).LoadSession(ctx)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(blob, raw))
}

func TestFirestoreSweepPreservesRewrittenGrant(t *testing.T) {
	b := NewTestFirestore(t)
	ctx := t.Context()
	expired := GrantRecord{ExpiresAt: time.Now().Add(-time.Hour)}
	require.NoError(t, b.StoreGrant(ctx, testGrantFamily, expired, 0))
	snapshot, err := b.client.Collection(firestoreGrants).Doc(testGrantFamily).Get(ctx)
	require.NoError(t, err)
	_, version, err := b.LoadGrant(ctx, testGrantFamily)
	require.NoError(t, err)
	live := GrantRecord{ExpiresAt: time.Now().Add(time.Hour).UTC()}
	require.NoError(t, b.StoreGrant(ctx, testGrantFamily, live, version))
	deleted, err := b.sweepGrant(ctx, snapshot, time.Now())
	require.NoError(t, err)
	assert.False(t, deleted)
	got, _, err := b.LoadGrant(ctx, testGrantFamily)
	require.NoError(t, err)
	assert.Equal(t, live, got)
}

func TestFirestoreEmptyAndMalformedSession(t *testing.T) {
	b := NewTestFirestore(t)
	ctx := t.Context()
	require.NoError(t, b.Session(42, testSID).StoreSession(ctx, nil))
	exists, err := b.Exists(ctx, 42, testSID)
	require.NoError(t, err)
	assert.False(t, exists)
	_, err = b.Session(42, testSID).LoadSession(ctx)
	require.ErrorIs(t, err, session.ErrNotFound)
	_, err = b.client.Collection(firestoreSessions).Doc(sessionBase(42, testSID)).Set(ctx, map[string]any{"data": "not bytes"})
	require.NoError(t, err)
	_, err = b.Session(42, testSID).LoadSession(ctx)
	require.Error(t, err, "malformed document must not look like a fresh login")
}

func TestFirestoreListingValidatesMetadata(t *testing.T) {
	b := NewTestFirestore(t)
	ctx := t.Context()
	_, err := b.client.Collection(firestoreSessions).Doc("foreign").Set(ctx, map[string]any{"data": []byte("not ours")})
	require.NoError(t, err)
	refs, err := b.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, refs)
	_, err = b.client.Collection(firestoreSessions).Doc(sessionBase(42, testSID)).Set(ctx, map[string]any{"data": []byte("x")})
	require.NoError(t, err)
	_, err = b.List(ctx)
	require.ErrorContains(t, err, "no modification time")
}

func TestFirestoreListingIgnoresPayload(t *testing.T) {
	b := NewTestFirestore(t)
	ctx := t.Context()
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, collection := range []string{firestoreSessions, firestoreRevoked} {
		_, err := b.client.Collection(collection).Doc(sessionBase(42, testSID)).Set(ctx, map[string]any{"data": "malformed payload", "updated_at": updated})
		require.NoError(t, err)
		_, err = b.client.Collection(collection).Doc(sessionBase(43, testSID)).Set(ctx, map[string]any{"data": []byte("other session"), "updated_at": updated})
		require.NoError(t, err)
	}
	for name, list := range map[string]func(context.Context) ([]SessionRef, error){"sessions": b.List, "tombstones": b.ListRevoked} {
		t.Run(name, func(t *testing.T) {
			refs, err := list(ctx)
			require.NoError(t, err, "a malformed payload must not block lifecycle listing")
			require.Len(t, refs, 2)
			for _, ref := range refs {
				assert.Equal(t, updated, ref.UpdatedAt)
			}
		})
	}
}

func TestFirestorePayloadIgnoresMetadata(t *testing.T) {
	b := NewTestFirestore(t)
	ctx := t.Context()
	blob := []byte("ciphertext")
	for name, fields := range map[string]map[string]any{
		"malformed metadata": {"data": blob, "updated_at": "not a timestamp"},
		"missing metadata":   {"data": blob},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := b.client.Collection(firestoreSessions).Doc(sessionBase(42, testSID)).Set(ctx, fields)
			require.NoError(t, err)
			got, err := b.Session(42, testSID).LoadSession(ctx)
			require.NoError(t, err, "payload reads must not decode lifecycle-only metadata")
			assert.Equal(t, blob, got)
		})
	}
}

func TestFirestoreCallerControlsLifetime(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("requires Firestore emulator")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store, err := NewFirestore(ctx, "demo-lifetime-"+NewSID(), "(default)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	cancel()
	require.NoError(t, store.Session(42, testSID).StoreSession(t.Context(), []byte("after root cancellation")))
	require.NoError(t, store.Close())
	_, err = store.List(t.Context())
	require.Error(t, err, "explicit cleanup must release the client")
}

func TestFirestoreConstructorValidation(t *testing.T) {
	_, err := NewFirestore(t.Context(), "", "db")
	require.ErrorContains(t, err, "required")
	_, err = NewFirestore(t.Context(), "p", "")
	require.ErrorContains(t, err, "required")
}

func TestFirestoreCancelledOperations(t *testing.T) {
	b := NewTestFirestore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := b.List(ctx)
	require.Error(t, err)
	_, _, err = b.LoadGrant(ctx, testGrantFamily)
	require.Error(t, err)
	require.Error(t, b.StoreGrant(ctx, testGrantFamily, GrantRecord{}, 0))
	require.Error(t, b.Revoke(ctx, 42, testSID))
	require.Error(t, b.Delete(ctx, 42, testSID))
	require.Error(t, b.DeleteRevoked(ctx, 42, testSID))
	_, err = b.Revoked(ctx, 42, testSID)
	require.Error(t, err)
	_, err = b.Exists(ctx, 42, testSID)
	require.Error(t, err)
	_, err = b.ListRevoked(ctx)
	require.Error(t, err)
	require.Error(t, b.Session(42, testSID).StoreSession(ctx, []byte("x")))
}
