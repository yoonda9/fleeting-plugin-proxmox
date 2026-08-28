package plugin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
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
