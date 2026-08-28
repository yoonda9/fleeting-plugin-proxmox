package plugin

import (
	"context"
	"encoding/json"
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

func TestUnmarshallingPluginSettings(t *testing.T) {
	settingsJSON := `{"url":"sample_url","template_id": 5}`
	instance := InstanceGroup{}

	err := json.Unmarshal([]byte(settingsJSON), &instance)
	require.NoError(t, err)

	require.Equal(t, "sample_url", instance.URL)
	require.Equal(t, 5, *instance.TemplateID)
}

func TestShutdownIsIdempotent(t *testing.T) {
	ig := &InstanceGroup{
		collectorShutdownTrigger:              make(chan struct{}, 1),
		sessionTicketRefresherShutdownTrigger: make(chan struct{}, 1),
	}

	ig.collectorWaitGroup.Go(func() {
		<-ig.collectorShutdownTrigger
	})

	ig.sessionTicketRefresherWaitGroup.Go(func() {
		<-ig.sessionTicketRefresherShutdownTrigger
	})

	require.NoError(t, ig.Shutdown(context.Background()))

	// Call Shutdown twice more: with 1-capacity trigger channels, a second
	// blocking send would still fit in the empty buffer, so it takes a third
	// call to prove the fix rather than merely reuse leftover buffer space.
	for i := 0; i < 2; i++ {
		done := make(chan error, 1)

		go func() {
			done <- ig.Shutdown(context.Background())
		}()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatalf("Shutdown call #%d did not return: possible deadlock", i+2)
		}
	}
}

// TestHeartbeat_toleratesTransientAgentBlip is a regression test:
// Heartbeat's retryIdempotent closure used to wrap vm.AgentOsInfo's error
// before classifyError ever saw it, which silently defeated the
// strings.HasPrefix 500/501 check and made a real PVE 500 (e.g. a QMP
// get-osinfo timeout under load) declare a healthy instance unhealthy on
// the first bad poll instead of retrying.
func TestHeartbeat_toleratesTransientAgentBlip(t *testing.T) {
	const vmid = 100

	const node = "pve-node"

	var agentRequests atomic.Int64

	mux := http.NewServeMux()

	mux.HandleFunc("/pools/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":[{"poolid":"test-pool","members":[{"id":"qemu/%d","type":"qemu","vmid":%d,"node":"%s"}]}]}`, vmid, vmid, node)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/status", node), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{}}`)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vmid), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"vmid":%d,"status":"running"}}`, vmid)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{}}`)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/agent/get-osinfo", node, vmid), func(w http.ResponseWriter, _ *http.Request) {
		if agentRequests.Add(1) <= 2 {
			// A bare 500 with a *valid* JSON body: go-proxmox's handleResponse
			// returns before reading the body for 500/501, so classTransient must
			// come from the status prefix, not an incidental decode failure.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"data":{}}`)

			return
		}

		fmt.Fprint(w, `{"data":{"result":{"hostname":"vm-100"}}}`)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	retryAttempts := 3
	ig := InstanceGroup{
		proxmox: proxmox.NewClient(server.URL),
		Settings: Settings{
			Pool:                    "test-pool",
			ProxmoxAPIRetryAttempts: &retryAttempts,
		},
		log: hclog.NewNullLogger(),
	}

	err := ig.Heartbeat(context.Background(), fmt.Sprintf("%d", vmid))

	require.NoError(t, err)
	require.Equal(t, int64(3), agentRequests.Load())
}
