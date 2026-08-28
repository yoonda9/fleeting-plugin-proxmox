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

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
)

// rawStatusHandler writes a raw HTTP/1.1 response whose *status line* (not just its body) is
// statusLine. Proxmox reports every refusal as a 500 whose reason phrase carries the actual
// message, and go-proxmox's handleResponse turns that straight into errors.New(res.Status);
// Go's http.ResponseWriter has no API to set a custom reason phrase, so this hijacks the
// connection to write the real wire format instead of approximating it.
func rawStatusHandler(statusLine string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			panic("test server does not support hijacking")
		}

		conn, _, err := hijacker.Hijack()
		if err != nil {
			panic(err)
		}
		defer conn.Close()

		fmt.Fprintf(conn, "HTTP/1.1 %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", statusLine)
	}
}

func agentOsInfoSuccessHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"data":{"result":{"hostname":"vm-100"}}}`)
}

// newTestVM is a VirtualMachine on the fake node, bound to client, for calling VM methods
// directly.
func newTestVM(client *proxmox.Client, vmid int) *proxmox.VirtualMachine {
	vm := &proxmox.VirtualMachine{}
	vm.New(client, "pve-node", vmid)

	return vm
}

func TestInstanceGroup_waitForAgent_succeedsAfterAgentNotReady(t *testing.T) {
	t.Parallel()

	const notReadyResponses = 2

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) <= notReadyResponses {
			rawStatusHandler(agentNotRunningMessage)(w, r)
			return
		}

		agentOsInfoSuccessHandler(w, r)
	}))
	defer server.Close()

	ig := newWaitTestGroup()
	*ig.InstanceAgentStartTimeout = 10

	err := ig.waitForAgent(context.Background(), newTestVM(proxmox.NewClient(server.URL), 100))

	require.NoError(t, err)
	require.GreaterOrEqual(t, requests.Load(), int64(notReadyResponses+1))
}

func TestInstanceGroup_waitForAgent_failsOnTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(terminalBadRequestHandler))
	defer server.Close()

	err := newWaitTestGroup().waitForAgent(context.Background(), newTestVM(proxmox.NewClient(server.URL), 100))

	require.Error(t, err)
	require.False(t, errors.Is(err, ErrAgentStartTimeout))
}

func TestInstanceGroup_waitForAgent_timesOut(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		statusLine string
	}{
		{
			// The agent is still booting: waiting is right, and the wait times out only
			// because this fake never lets it finish.
			name:       "agent still booting",
			statusLine: agentNotRunningMessage,
		},
		{
			// A permanent misconfiguration Proxmox also reports as a 500, so it is
			// indistinguishable from the case above and burns the whole budget too. The
			// error it returns is the operator's only sight of the reason.
			name:       "no guest agent configured",
			statusLine: "500 No QEMU guest agent configured",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(rawStatusHandler(testCase.statusLine))
			defer server.Close()

			started := time.Now()
			err := newWaitTestGroup().waitForAgent(context.Background(), newTestVM(proxmox.NewClient(server.URL), 100))
			elapsed := time.Since(started)

			require.ErrorIs(t, err, ErrAgentStartTimeout)
			require.ErrorContains(t, err, testCase.statusLine,
				"the timeout must carry what Proxmox actually said, or the reason is invisible")
			require.Less(t, elapsed, 5*time.Second)
		})
	}
}
