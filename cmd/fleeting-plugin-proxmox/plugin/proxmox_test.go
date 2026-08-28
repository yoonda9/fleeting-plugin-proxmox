package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newSampleCredentialsFile writes a valid Proxmox VE credentials file to a temp dir and
// returns its path.
func newSampleCredentialsFile(t *testing.T) string {
	t.Helper()

	credentialsPath := path.Join(t.TempDir(), "prox_credentials.json")

	err := os.WriteFile(
		credentialsPath,
		[]byte(`{"realm": "pve","username": "03Ewl6rENi","password": "-rx£N503o_8(%\"l+=*4,YD"}`),
		0o600,
	)
	require.NoError(t, err)

	return credentialsPath
}

// ticketHandler answers POST /api2/json/access/ticket with a valid session, so that
// proxmox.WithEagerAuth's login call at client construction succeeds fast.
func ticketHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"data":{"ticket":"tkt","CSRFPreventionToken":"csrf","username":"u"}}`)
}

// newClientTestGroup wires an InstanceGroup to an httptest Proxmox served by mux, which
// also answers the login proxmox.WithEagerAuth performs at client construction.
func newClientTestGroup(t *testing.T, mux *http.ServeMux, httpTimeout int) *InstanceGroup {
	t.Helper()

	mux.HandleFunc("/api2/json/access/ticket", ticketHandler)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ig := newWaitTestGroup()
	ig.URL = server.URL
	ig.CredentialsFilePath = newSampleCredentialsFile(t)
	*ig.HTTPTimeout = httpTimeout

	return ig
}

func TestInstanceGroup_getProxmoxClient(t *testing.T) {
	ig := newClientTestGroup(t, http.NewServeMux(), DefaultHTTPTimeout)

	_, err := ig.getProxmoxClient()
	require.NoError(t, err)
}

func TestInstanceGroup_getProxmoxClient_httpTimeout(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/version", func(_ http.ResponseWriter, r *http.Request) {
		// Outlast the client's one-second timeout, but return as soon as the client hangs up
		// so that closing the server does not wait out the whole sleep.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})

	client, err := newClientTestGroup(t, mux, 1).getProxmoxClient()
	require.NoError(t, err)

	started := time.Now()
	_, err = client.Version(t.Context())
	elapsed := time.Since(started)

	require.Error(t, err)
	require.Less(t, elapsed, 2*time.Second, "client should have timed out instead of waiting for the full handler sleep")
}

func TestInstanceGroup_getProxmoxCredentials(t *testing.T) {
	tempDir := t.TempDir()
	ig := InstanceGroup{
		Settings: Settings{
			CredentialsFilePath: path.Join(tempDir, "sample_credentials.json"),
		},
	}

	// Missing credentials file
	_, err := ig.getProxmoxCredentials()
	require.ErrorIs(t, err, os.ErrNotExist)

	// Malformed credentials file
	err = os.WriteFile(
		ig.CredentialsFilePath,
		[]byte(`{"realm": 'pve',`),
		0o600,
	)
	require.NoError(t, err)

	_, err = ig.getProxmoxCredentials()
	require.Error(t, err)

	// Correct credentials file
	err = os.WriteFile(
		ig.CredentialsFilePath,
		[]byte(`{"realm": "pve","username": "oQcW8N246FODI6Qui","password": "88u3[kKLJ{gU7A£fhWq"}`),
		0o600,
	)
	require.NoError(t, err)

	credentials, err := ig.getProxmoxCredentials()
	require.NoError(t, err)
	require.Equal(t, "pve", credentials.Realm)
	require.Equal(t, "oQcW8N246FODI6Qui", credentials.Username)
	require.Equal(t, `88u3[kKLJ{gU7A£fhWq`, credentials.Password)
}

// retryTestErrors returns a transient and a terminal error as go-proxmox really produces them,
// so the retry tests drive retryIdempotent with the values classifyError actually sees.
func retryTestErrors(t *testing.T) (transient, terminal error) {
	t.Helper()

	return realClassifyErrorFixture(t, transientHTMLHandler, 0), realClassifyErrorFixture(t, terminalBadRequestHandler, 0)
}

func TestRetryIdempotent_retriesTransientThenSucceeds(t *testing.T) {
	transientErr, _ := retryTestErrors(t)

	var calls int

	err := retryIdempotent(t.Context(), 3, time.Millisecond, func() error {
		calls++
		if calls <= 2 {
			return transientErr
		}

		return nil
	})

	require.NoError(t, err)
	require.Equal(t, 3, calls)
}

func TestRetryIdempotent_doesNotRetryTerminal(t *testing.T) {
	_, terminalErr := retryTestErrors(t)

	var calls int

	err := retryIdempotent(t.Context(), 3, time.Millisecond, func() error {
		calls++

		return terminalErr
	})

	require.ErrorIs(t, err, terminalErr)
	require.Equal(t, 1, calls)
}

// A non-positive attempts count still calls fn exactly once, and reports what it returned,
// rather than no-oping to a silent success.
func TestRetryIdempotent_nonPositiveAttemptsCallsFnOnce(t *testing.T) {
	sentinel := errors.New("boom")

	testCases := []struct {
		name     string
		attempts int
		fnErr    error
	}{
		{"zero attempts", 0, nil},
		{"negative attempts", -5, nil},
		{"zero attempts reports the failure", 0, sentinel},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var calls atomic.Int64

			err := retryIdempotent(t.Context(), testCase.attempts, time.Millisecond, func() error {
				calls.Add(1)

				return testCase.fnErr
			})

			require.ErrorIs(t, err, testCase.fnErr)
			require.Equal(t, int64(1), calls.Load(), "fn must be called exactly once")
		})
	}
}

func TestBackoffForAttempt(t *testing.T) {
	tests := []struct {
		name    string
		base    time.Duration
		attempt int
		want    time.Duration
	}{
		{"first attempt returns base", time.Second, 0, time.Second},
		{"doubles per attempt", time.Second, 2, 4 * time.Second},
		{"caps at proxmoxRetryMaxBackoff", time.Second, 10, proxmoxRetryMaxBackoff},
		{"stays capped for a huge attempt", time.Second, 100, proxmoxRetryMaxBackoff},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := backoffForAttempt(tt.base, tt.attempt)

			require.Equal(t, tt.want, got)
			require.Positive(t, got)
		})
	}
}

func TestRetryIdempotent_stopsOnCancelledContext(t *testing.T) {
	transientErr, _ := retryTestErrors(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var calls int

	err := retryIdempotent(ctx, 5, time.Millisecond, func() error {
		calls++
		// Simulate the caller's context dying right after the first attempt:
		// the retry loop must notice and stop instead of retrying up to attempts.
		cancel()

		return transientErr
	})

	require.Error(t, err)
	require.Equal(t, 1, calls)
}

// attempts is a total call count, the first call included - what the setting's own doc comment
// and the README both promise - so attempts=1 must not retry at all.
func TestRetryIdempotent_attemptsCountsTheFirstCall(t *testing.T) {
	transientErr, _ := retryTestErrors(t)

	for _, attempts := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("%d attempts", attempts), func(t *testing.T) {
			var calls int

			err := retryIdempotent(t.Context(), attempts, time.Millisecond, func() error {
				calls++

				return transientErr
			})

			require.ErrorIs(t, err, transientErr)
			require.Equal(t, attempts, calls, "attempts must count the first call")
		})
	}
}
