package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/gotd/td/session"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

const (
	firestoreSessions = "sessions-v3"
	firestoreRevoked  = "revoked-v3"
	firestoreGrants   = "oauth-v3-grants"
)

// Firestore persists ciphertext sessions, revocation tombstones and refresh
// grants in a dedicated database. Server SDK access is controlled by IAM.
type Firestore struct{ client *firestore.Client }

// NewFirestore probes read access at startup. The caller must Close the store
// after all consumers have stopped; cancelling ctx does not close the client.
func NewFirestore(ctx context.Context, project, database string) (*Firestore, error) {
	if project == "" || database == "" {
		return nil, fmt.Errorf("sessionstore: Firestore project and database are required")
	}
	client, err := firestore.NewClientWithDatabase(ctx, project, database)
	if err != nil {
		return nil, fmt.Errorf("sessionstore: creating Firestore client: %w", err)
	}
	it := client.Collection(firestoreSessions).Limit(1).Documents(ctx)
	_, err = it.Next()
	it.Stop()
	if err != nil && !errors.Is(err, iterator.Done) {
		_ = client.Close()
		return nil, fmt.Errorf("sessionstore: probing Firestore database: %w", err)
	}
	return &Firestore{client: client}, nil
}

// Close releases the client after HTTP drain and its consumers have stopped.
func (f *Firestore) Close() error {
	if err := f.client.Close(); err != nil {
		return fmt.Errorf("sessionstore: closing Firestore client: %w", err)
	}
	return nil
}

func (f *Firestore) Session(userID tgid.UserID, sid string) session.Storage {
	return firestoreSession{doc: f.client.Collection(firestoreSessions).Doc(sessionBase(userID, sid))}
}

func (f *Firestore) Exists(ctx context.Context, userID tgid.UserID, sid string) (bool, error) {
	_, err := f.Session(userID, sid).LoadSession(ctx)
	if errors.Is(err, session.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sessionstore: checking session: %w", err)
	}
	return true, nil
}

func (f *Firestore) Delete(ctx context.Context, userID tgid.UserID, sid string) error {
	return deleteDocument(ctx, f.client.Collection(firestoreSessions).Doc(sessionBase(userID, sid)))
}

func deleteDocument(ctx context.Context, doc *firestore.DocumentRef) error {
	if _, err := doc.Delete(ctx); err != nil {
		return fmt.Errorf("sessionstore: deleting document: %w", err)
	}
	return nil
}

func (f *Firestore) List(ctx context.Context) ([]SessionRef, error) {
	return f.list(ctx, firestoreSessions)
}
func (f *Firestore) ListRevoked(ctx context.Context) ([]SessionRef, error) {
	return f.list(ctx, firestoreRevoked)
}

func (f *Firestore) list(ctx context.Context, collection string) ([]SessionRef, error) {
	it := f.client.Collection(collection).Select("updated_at").Documents(ctx)
	defer it.Stop()
	var refs []SessionRef
	for {
		doc, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return refs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("sessionstore: listing %s: %w", collection, err)
		}
		userID, sid, ok := parseSessionBase(doc.Ref.ID)
		if !ok {
			continue
		}
		// The original imported timestamp, not Firestore UpdateTime, governs orphan age.
		var data struct {
			UpdatedAt time.Time `firestore:"updated_at"`
		}
		if err := doc.DataTo(&data); err != nil {
			return nil, fmt.Errorf("sessionstore: decoding %s metadata: %w", collection, err)
		}
		if data.UpdatedAt.IsZero() {
			return nil, fmt.Errorf("sessionstore: %s document has no modification time", collection)
		}
		refs = append(refs, SessionRef{UserID: userID, SID: sid, UpdatedAt: data.UpdatedAt})
	}
}

func (f *Firestore) Revoke(ctx context.Context, userID tgid.UserID, sid string) error {
	// Keep the tombstone separate: a live gotd client can re-store the blob.
	err := f.client.RunTransaction(ctx, func(_ context.Context, tx *firestore.Transaction) error {
		if err := tx.Set(f.client.Collection(firestoreRevoked).Doc(sessionBase(userID, sid)), map[string]any{"updated_at": firestore.ServerTimestamp}); err != nil {
			return fmt.Errorf("writing tombstone: %w", err)
		}
		if err := tx.Delete(f.client.Collection(firestoreSessions).Doc(sessionBase(userID, sid))); err != nil {
			return fmt.Errorf("deleting session: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sessionstore: revoking session: %w", err)
	}
	return nil
}

func (f *Firestore) Revoked(ctx context.Context, userID tgid.UserID, sid string) (bool, error) {
	_, err := f.client.Collection(firestoreRevoked).Doc(sessionBase(userID, sid)).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sessionstore: probing revocation: %w", err)
	}
	return true, nil
}

func (f *Firestore) DeleteRevoked(ctx context.Context, userID tgid.UserID, sid string) error {
	return deleteDocument(ctx, f.client.Collection(firestoreRevoked).Doc(sessionBase(userID, sid)))
}

// The grant payload keeps the existing JSON contract. It carries no secret.
// The document timestamp is an opaque CAS version; transactions compare it
// with the current document before writing, including on transaction retries.
func decodeGrant(doc *firestore.DocumentSnapshot) (GrantRecord, error) {
	var data struct {
		Data []byte `firestore:"data"`
	}
	if err := doc.DataTo(&data); err != nil {
		return GrantRecord{}, fmt.Errorf("%w: %w", errUndecodableGrant, err)
	}
	var grant GrantRecord
	if err := json.Unmarshal(data.Data, &grant); err != nil {
		return GrantRecord{}, fmt.Errorf("%w: %w", errUndecodableGrant, err)
	}
	return grant, nil
}

func (f *Firestore) LoadGrant(ctx context.Context, family string) (GrantRecord, int64, error) {
	doc, err := f.client.Collection(firestoreGrants).Doc(family).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return GrantRecord{}, 0, nil
	}
	if err != nil {
		return GrantRecord{}, 0, fmt.Errorf("sessionstore: reading grant: %w", err)
	}
	grant, err := decodeGrant(doc)
	return grant, doc.UpdateTime.UnixNano(), err
}

func (f *Firestore) StoreGrant(ctx context.Context, family string, grant GrantRecord, version int64) error {
	data, err := json.Marshal(grant)
	if err != nil {
		return fmt.Errorf("sessionstore: encoding grant: %w", err)
	}
	ref := f.client.Collection(firestoreGrants).Doc(family)
	err = f.client.RunTransaction(ctx, func(_ context.Context, tx *firestore.Transaction) error {
		doc, err := tx.Get(ref)
		var current GrantRecord
		var currentVersion int64
		switch {
		case status.Code(err) == codes.NotFound:
		case err != nil:
			return fmt.Errorf("reading current grant: %w", err)
		default:
			current, err = decodeGrant(doc)
			if err != nil {
				return err
			}
			currentVersion = doc.UpdateTime.UnixNano()
		}
		if version != currentVersion {
			return &GrantConflictError{Current: current, Version: currentVersion}
		}
		if err := tx.Set(ref, map[string]any{"data": data, "expires_at": grant.ExpiresAt}); err != nil {
			return fmt.Errorf("writing grant: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sessionstore: storing grant: %w", err)
	}
	return nil
}

func (f *Firestore) SweepAuthState(ctx context.Context, now time.Time) ([]string, error) {
	it := f.client.Collection(firestoreGrants).Documents(ctx)
	defer it.Stop()
	var undecodable []string
	var errs []error
	for {
		if err := ctx.Err(); err != nil {
			return undecodable, errors.Join(append(errs, err)...)
		}
		doc, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return undecodable, errors.Join(errs...)
		}
		if err != nil {
			return undecodable, errors.Join(append(errs, fmt.Errorf("sessionstore: listing grants: %w", err))...)
		}
		if !ValidSID(doc.Ref.ID) {
			continue
		}
		deleted, err := f.sweepGrant(ctx, doc, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("sessionstore: sweeping grant %s: %w", doc.Ref.ID, err))
		}
		if deleted {
			undecodable = append(undecodable, doc.Ref.ID)
		}
	}
}

func (f *Firestore) sweepGrant(ctx context.Context, doc *firestore.DocumentSnapshot, now time.Time) (bool, error) {
	grant, err := decodeGrant(doc)
	undecodable := errors.Is(err, errUndecodableGrant)
	if !undecodable && !grant.Expired(now) {
		return false, nil
	}
	_, err = doc.Ref.Delete(ctx, firestore.LastUpdateTime(doc.UpdateTime))
	if status.Code(err) == codes.FailedPrecondition || status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("deleting grant: %w", err)
	}
	return undecodable, nil
}

type firestoreSession struct{ doc *firestore.DocumentRef }

func (s firestoreSession) LoadSession(ctx context.Context) ([]byte, error) {
	doc, err := s.doc.Get(ctx)
	if status.Code(err) == codes.NotFound {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sessionstore: reading session: %w", err)
	}
	var data struct {
		Data []byte `firestore:"data"`
	}
	if err := doc.DataTo(&data); err != nil {
		return nil, fmt.Errorf("sessionstore: decoding session: %w", err)
	}
	if len(data.Data) == 0 {
		return nil, session.ErrNotFound
	}
	return data.Data, nil
}

func (s firestoreSession) StoreSession(ctx context.Context, data []byte) error {
	if _, err := s.doc.Set(ctx, map[string]any{"data": data, "updated_at": firestore.ServerTimestamp}); err != nil {
		return fmt.Errorf("sessionstore: writing session: %w", err)
	}
	return nil
}
