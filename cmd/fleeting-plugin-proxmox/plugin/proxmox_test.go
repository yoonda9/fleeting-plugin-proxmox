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

	"github.com/luthermonson/go-proxmox"
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

func TestInstanceGroup_getProxmoxClient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/access/ticket", ticketHandler)
	server := httptest.NewServer(mux)
	defer server.Close()

	httpTimeout := DefaultHTTPTimeout
	httpMaxIdleConnsPerHost := DefaultHTTPMaxIdleConnsPerHost
	ig := InstanceGroup{
		Settings: Settings{
			URL:                     server.URL,
			InsecureSkipTLSVerify:   false,
			CredentialsFilePath:     newSampleCredentialsFile(t),
			HTTPTimeout:             &httpTimeout,
			HTTPMaxIdleConnsPerHost: &httpMaxIdleConnsPerHost,
		},
	}

	_, err := ig.getProxmoxClient()
	require.NoError(t, err)
}

func TestInstanceGroup_getProxmoxClient_httpTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/access/ticket", ticketHandler)
	mux.HandleFunc("/api2/json/version", func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	httpTimeout := 1
	httpMaxIdleConnsPerHost := DefaultHTTPMaxIdleConnsPerHost
	ig := InstanceGroup{
		Settings: Settings{
			URL:                     server.URL,
			CredentialsFilePath:     newSampleCredentialsFile(t),
			HTTPTimeout:             &httpTimeout,
			HTTPMaxIdleConnsPerHost: &httpMaxIdleConnsPerHost,
		},
	}

	client, err := ig.getProxmoxClient()
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

func TestRetryIdempotent_retriesTransientThenSucceeds(t *testing.T) {
	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("<html>Service Unavailable</html>"))

			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{}}`)
	}))
	defer server.Close()

	client := proxmox.NewClient(server.URL)

	err := retryIdempotent(t.Context(), 3, time.Millisecond, func() error {
		return client.Get(t.Context(), "/some/path", &struct{}{})
	})

	require.NoError(t, err)
	require.Equal(t, int64(3), requests.Load())
}

func TestRetryIdempotent_doesNotRetryTerminal(t *testing.T) {
	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"data":null,"errors":{"foo":"bar"}}`))
	}))
	defer server.Close()

	client := proxmox.NewClient(server.URL)

	err := retryIdempotent(t.Context(), 3, time.Millisecond, func() error {
		return client.Get(t.Context(), "/some/path", &struct{}{})
	})

	require.Error(t, err)
	require.Equal(t, int64(1), requests.Load())
}

func TestRetryIdempotent_nonPositiveAttemptsStillCallsFnOnce(t *testing.T) {
	for _, attempts := range []int{0, -1, -5} {
		t.Run(fmt.Sprintf("attempts=%d", attempts), func(t *testing.T) {
			var calls atomic.Int64

			err := retryIdempotent(t.Context(), attempts, time.Millisecond, func() error {
				calls.Add(1)

				return nil
			})

			require.NoError(t, err)
			require.Equal(t, int64(1), calls.Load(), "fn must be called at least once, never no-op'd")
		})
	}
}

func TestRetryIdempotent_nonPositiveAttemptsPropagatesError(t *testing.T) {
	var calls atomic.Int64

	sentinel := errors.New("boom")

	err := retryIdempotent(t.Context(), 0, time.Millisecond, func() error {
		calls.Add(1)

		return sentinel
	})

	require.ErrorIs(t, err, sentinel)
	require.Equal(t, int64(1), calls.Load())
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
		{"caps instead of overflowing for a huge attempt", time.Second, 100, proxmoxRetryMaxBackoff},
		{"caps instead of overflowing right at the shift width", time.Second, 63, proxmoxRetryMaxBackoff},
		{"never negative for a negative attempt", time.Second, -1, proxmoxRetryMaxBackoff},
		{"caps instead of wrapping to zero at production base", proxmoxRetryBackoff, 56, proxmoxRetryMaxBackoff},
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
	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<html>Service Unavailable</html>"))
	}))
	defer server.Close()

	client := proxmox.NewClient(server.URL)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	err := retryIdempotent(ctx, 5, time.Millisecond, func() error {
		err := client.Get(ctx, "/some/path", &struct{}{})
		// Simulate the caller's context dying right after the first attempt:
		// the retry loop must notice and stop instead of retrying up to attempts.
		cancel()

		return err
	})

	require.Error(t, err)
	require.Equal(t, int64(1), requests.Load())
}
