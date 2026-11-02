package tgclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"rsc.io/qr"
)

// scriptedQRFlow replays a fixed sequence of snapshots, one per Observe, and
// records the passwords submitted to it.
type scriptedQRFlow struct {
	mu        sync.Mutex
	snapshots []QRSnapshot
	result    QRResult
	passwords []string
	aborted   bool
}

func (f *scriptedQRFlow) Observe() QRSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.snapshots[0]
	if len(f.snapshots) > 1 {
		f.snapshots = f.snapshots[1:]
	}
	return s
}

func (f *scriptedQRFlow) Result() (QRResult, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.result, f.snapshots[0].State == QRDone
}

func (f *scriptedQRFlow) SubmitPassword(pw string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.passwords = append(f.passwords, pw)
	return true
}

func (f *scriptedQRFlow) Abort() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = true
}

// waitingFor is a QRWaiting snapshot showing url as QR revision rev.
func waitingFor(url string, rev int) QRSnapshot {
	s := QRSnapshot{State: QRWaiting, QRRevision: rev}
	s.TokenURL = url
	return s
}

func TestDriveQRLoginShowsEachTokenOnceAndReturnsSession(t *testing.T) {
	flow := &scriptedQRFlow{
		snapshots: []QRSnapshot{
			waitingFor("tg://login?qr=one", 1),
			waitingFor("tg://login?qr=one", 1),
			waitingFor("tg://login?qr=two", 2),
			{State: QRDone},
		},
		result: QRResult{User: QRUser{ID: 42, Username: "alice"}, SessionData: []byte("session")},
	}
	var out bytes.Buffer
	prompt := newUserAuthenticator("", strings.NewReader(""), &out)

	got, err := driveQRLogin(t.Context(), flow, prompt, &out, time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, []byte("session"), got.SessionData)
	assert.Equal(t, 2, strings.Count(out.String(), "Link Desktop Device"), "each new token is shown exactly once")
	assert.Empty(t, flow.passwords)
}

func TestDriveQRLoginAsksForPasswordAndReportsRejection(t *testing.T) {
	flow := &scriptedQRFlow{
		snapshots: []QRSnapshot{
			{State: QRPasswordNeeded, AttemptsLeft: 3},
			{State: QRPasswordVerifying},
			{State: QRPasswordNeeded, Rejected: 1, AttemptsLeft: 2},
			{State: QRPasswordVerifying},
			{State: QRDone},
		},
		result: QRResult{SessionData: []byte("session")},
	}
	var out bytes.Buffer
	prompt := newUserAuthenticator("", strings.NewReader("wrong\nright\n"), &out)

	_, err := driveQRLogin(t.Context(), flow, prompt, &out, time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, []string{"wrong", "right"}, flow.passwords)
	assert.Contains(t, out.String(), "Wrong 2FA password, 2 attempt(s) left.")
}

func TestDriveQRLoginReturnsFlowFailure(t *testing.T) {
	errExpired := errors.New("token expired")
	flow := &scriptedQRFlow{snapshots: []QRSnapshot{{State: QRFailed, Err: errExpired}}}
	var out bytes.Buffer

	_, err := driveQRLogin(t.Context(), flow, newUserAuthenticator("", strings.NewReader(""), &out), &out, time.Millisecond)
	require.ErrorIs(t, err, errExpired)
}

func TestDriveQRLoginStopsOnCancel(t *testing.T) {
	flow := &scriptedQRFlow{snapshots: []QRSnapshot{{State: QRWaiting}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer

	_, err := driveQRLogin(ctx, flow, newUserAuthenticator("", strings.NewReader(""), &out), &out, time.Millisecond)
	require.ErrorIs(t, err, context.Canceled)
}

func TestDriveQRLoginCancelsWhileReadingPassword(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	in, writer := io.Pipe()
	t.Cleanup(func() {
		require.NoError(t, in.Close())
		require.NoError(t, writer.Close())
	})
	started := make(chan struct{})
	flow := &scriptedQRFlow{snapshots: []QRSnapshot{{State: QRPasswordNeeded, AttemptsLeft: 3}}}
	done := make(chan error, 1)
	go func() {
		_, err := driveQRLogin(ctx, flow, newUserAuthenticator("", &passwordReadNotifier{Reader: in, started: started}, io.Discard), io.Discard, time.Millisecond)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("QR login did not stop while waiting for the password")
	}
}

type passwordReadNotifier struct {
	io.Reader
	started chan struct{}
	once    sync.Once
}

func (r *passwordReadNotifier) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.Reader.Read(p) //nolint:wrapcheck // The notifier preserves the reader's result.
}

// TestRenderQRMatchesEncodedModules decodes the half-block drawing back into
// modules and compares them with the encoder's, quiet zone included.
func TestRenderQRMatchesEncodedModules(t *testing.T) {
	const url = "tg://login?token=AQIDBAUGBwgJCgsMDQ4PEA"
	var out bytes.Buffer
	require.NoError(t, renderQR(&out, url))

	code, err := qr.Encode(url, qr.M)
	require.NoError(t, err)
	const quiet = 4
	width := code.Size + 2*quiet

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	require.Len(t, lines, (width+1)/2)
	for row, line := range lines {
		line = strings.TrimPrefix(line, "\x1b[97;40m")
		line = strings.TrimSuffix(line, "\x1b[0m")
		cells := []rune(line)
		require.Len(t, cells, width, "line %d", row)
		for col, cell := range cells {
			x, y := col-quiet, 2*row-quiet
			wantTop, wantBottom := !qrBlack(code, x, y), !qrBlack(code, x, y+1)
			gotTop := cell == '█' || cell == '▀'
			gotBottom := cell == '█' || cell == '▄'
			assert.Equal(t, wantTop, gotTop, "module (%d,%d)", x, y)
			assert.Equal(t, wantBottom, gotBottom, "module (%d,%d)", x, y+1)
		}
	}
}

func qrBlack(code *qr.Code, x, y int) bool {
	return x >= 0 && y >= 0 && x < code.Size && y < code.Size && code.Black(x, y)
}

func TestQRUserName(t *testing.T) {
	assert.Equal(t, "@alice", qrUserName(QRUser{ID: 1, Username: "alice"}))
	assert.Equal(t, "user 7", qrUserName(QRUser{ID: 7}))
}
