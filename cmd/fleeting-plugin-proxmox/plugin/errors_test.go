package plugin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
)

// realClassifyErrorFixture builds an error the way go-proxmox's client actually
// produces it for the scenario described, against a real httptest server, so
// classifyError is exercised against real error values rather than hand-built
// approximations.
func realClassifyErrorFixture(t *testing.T, handler http.HandlerFunc, httpTimeout time.Duration) error {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := proxmox.NewClient(server.URL, proxmox.WithHTTPClient(&http.Client{Timeout: httpTimeout}))

	err := client.Get(t.Context(), "/some/path", &struct{}{})
	require.Error(t, err)

	return err
}

func TestClassifyError(t *testing.T) {
	timeoutErr := realClassifyErrorFixture(t, func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}, time.Millisecond)

	connRefusedErr := func() error {
		// Nothing is listening on this port: httptest.NewServer + Close leaves the
		// port free, so a dial against it fails with a real connection-refused error.
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
		server.Close()

		client := proxmox.NewClient(server.URL)

		return client.Get(t.Context(), "/some/path", &struct{}{})
	}()
	require.Error(t, connRefusedErr)

	malformedBodyErr := realClassifyErrorFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<html>Bad Gateway</html>"))
	}, 0)

	badRequestErr := realClassifyErrorFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"data":null,"errors":{"foo":"bar"}}`))
	}, 0)

	// handleResponse returns errors.New(res.Status) for 500/501 before ever
	// reading the body, so the body content is irrelevant here - it's included
	// only to prove this is classified via the status prefix, not by coincidence
	// of an unparsable body (as the malformed-body case above is).
	internalServerErrorErr := realClassifyErrorFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"data":{"result":"ok"}}`))
	}, 0)

	notImplementedErr := realClassifyErrorFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	}, 0)

	testCases := []struct {
		name     string
		err      error
		expected errorClass
	}{
		{"http client timeout", timeoutErr, classTransient},
		{"connection refused", connRefusedErr, classTransient},
		{"malformed body from an overloaded proxy", malformedBodyErr, classTransient},
		{"bad request", badRequestErr, classTerminal},
		{"internal server error (500)", internalServerErrorErr, classTransient},
		{"not implemented (501)", notImplementedErr, classTransient},
		{
			// Real Proxmox sends this as the raw HTTP/1.1 reason phrase (not just
			// the JSON body), which go-proxmox's handleResponse turns into
			// errors.New(res.Status) for any 500. httptest's ResponseWriter has no
			// way to set a custom reason phrase, so this fixture is built directly
			// rather than round-tripped - it is byte-for-byte what handleResponse
			// would produce for that response.
			"literal agent-not-running message",
			errors.New(agentNotRunningMessage),
			classAgentNotReady,
		},
		{"unrelated terminal error", errors.New("something else went wrong"), classTerminal},
		{"nil", nil, classTerminal},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, classifyError(tt.err))
		})
	}
}
