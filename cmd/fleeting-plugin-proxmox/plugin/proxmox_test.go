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
