package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
)

// agentNotReadyHandler writes a raw HTTP/1.1 response whose *status line* (not
// just its body) is the literal go-proxmox/Proxmox use for "guest agent isn't
// running yet". Real Proxmox sends this as the reason phrase of a 500, which is
// what go-proxmox's handleResponse turns straight into errors.New(res.Status); Go's
// http.ResponseWriter has no API to set a custom reason phrase, so this hijacks the
// connection to write the real wire format instead of approximating it.
func agentNotReadyHandler(w http.ResponseWriter, _ *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		panic("test server does not support hijacking")
	}

	conn, _, err := hijacker.Hijack()
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "HTTP/1.1 %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", agentNotRunningMessage)
}

func agentOsInfoSuccessHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"data":{"result":{"hostname":"vm-100"}}}`)
}

func newTestVM(t *testing.T, serverURL string) *proxmox.VirtualMachine {
	t.Helper()

	client := proxmox.NewClient(serverURL)
	vm := &proxmox.VirtualMachine{}
	vm.New(client, "pve-node", 100)

	return vm
}

func TestInstanceGroup_waitForAgent_succeedsAfterAgentNotReady(t *testing.T) {
	const notReadyResponses = 2

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) <= notReadyResponses {
			agentNotReadyHandler(w, r)
			return
		}

		agentOsInfoSuccessHandler(w, r)
	}))
	defer server.Close()

	agentStartTimeout := 10
	ig := InstanceGroup{
		Settings: Settings{InstanceAgentStartTimeout: &agentStartTimeout},
		log:      hclog.NewNullLogger(),
	}

	err := ig.waitForAgent(context.Background(), newTestVM(t, server.URL))

	require.NoError(t, err)
	require.GreaterOrEqual(t, requests.Load(), int64(notReadyResponses+1))
}

func TestInstanceGroup_waitForAgent_failsOnTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A bare 500/501 is classified as transient, so this must use a
		// status classifyError genuinely treats as terminal, e.g. a 400.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"data":null,"errors":{"foo":"bar"}}`))
	}))
	defer server.Close()

	agentStartTimeout := 10
	ig := InstanceGroup{
		Settings: Settings{InstanceAgentStartTimeout: &agentStartTimeout},
		log:      hclog.NewNullLogger(),
	}

	err := ig.waitForAgent(context.Background(), newTestVM(t, server.URL))

	require.Error(t, err)
	require.False(t, errors.Is(err, ErrAgentStartTimeout))
}

func TestInstanceGroup_waitForAgent_timesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(agentNotReadyHandler))
	defer server.Close()

	agentStartTimeout := 1
	ig := InstanceGroup{
		Settings: Settings{InstanceAgentStartTimeout: &agentStartTimeout},
		log:      hclog.NewNullLogger(),
	}

	started := time.Now()
	err := ig.waitForAgent(context.Background(), newTestVM(t, server.URL))
	elapsed := time.Since(started)

	require.ErrorIs(t, err, ErrAgentStartTimeout)
	require.Less(t, elapsed, 5*time.Second)
}
