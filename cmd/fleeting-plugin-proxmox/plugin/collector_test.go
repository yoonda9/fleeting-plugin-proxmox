package plugin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
)

func TestTriggerCollectionOnFullChannelReturnsImmediately(t *testing.T) {
	ig := &InstanceGroup{
		instanceCollectionTrigger: make(chan struct{}, 1),
	}

	// Fill the channel so a blocking send would stall forever.
	ig.instanceCollectionTrigger <- struct{}{}

	done := make(chan struct{})

	go func() {
		ig.triggerCollection()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("triggerCollection blocked on a full channel")
	}

	require.Len(t, ig.instanceCollectionTrigger, 1)
}

// TestInstanceGroup_collectInstance_releasesVMIDAfterConfirmedDelete is a
// regression test: collector.go's release-on-delete path had no test of its
// own, and it is the release trigger the vmid allocator's default-mode
// safety rests on.
func TestInstanceGroup_collectInstance_releasesVMIDAfterConfirmedDelete(t *testing.T) {
	const vmid = 105

	const node = "pve-node"

	const upid = proxmox.UPID("UPID:pve-node:00001A2B:00000000:00000000:qmdestroy:105:root@pam:")

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/status", node), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{}}`)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vmid), func(w http.ResponseWriter, _ *http.Request) {
		// "stopped" so collectInstance skips the Stop call entirely.
		fmt.Fprintf(w, `{"data":{"vmid":%d,"status":"stopped"}}`, vmid)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{}}`)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d", node, vmid), func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodDelete, r.Method)
		fmt.Fprintf(w, `{"data":%q}`, upid)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/tasks/%s/status", node, upid), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":{"upid":%q,"node":%q,"type":"qmdestroy","id":"%d","user":"root@pam","status":"stopped","exitstatus":"OK"}}`,
			upid, node, vmid)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	waitInterval := 1
	waitTimeout := 5

	ig := &InstanceGroup{
		proxmox: proxmox.NewClient(server.URL),
		Settings: Settings{
			ProxmoxTaskWaitInterval: &waitInterval,
			ProxmoxTaskWaitTimeout:  &waitTimeout,
		},
		log: hclog.NewNullLogger(),
		vmids: newVMIDAllocator(alwaysFreeCheckID, func(context.Context) (int, error) {
			return vmid, nil
		}),
	}

	reserved, err := ig.vmids.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, vmid, reserved)

	ig.collectInstance(context.Background(), proxmox.ClusterResource{
		VMID: uint64(vmid),
		Node: node,
		Type: proxmoxResourceTypeQemu,
	})

	// A confirmed delete must have released the vmid: a second Allocate must
	// be able to reissue it rather than treat it as still reserved.
	reallocated, err := ig.vmids.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, vmid, reallocated)
}

// TestInstanceGroup_collectInstance_doesNotReleaseVMIDWhenDeleteFails proves
// the sibling policy: a delete task that itself fails must leave the
// reservation in place instead of freeing an id that may still be live.
func TestInstanceGroup_collectInstance_doesNotReleaseVMIDWhenDeleteFails(t *testing.T) {
	const vmid = 105

	const node = "pve-node"

	const upid = proxmox.UPID("UPID:pve-node:00001A2B:00000000:00000000:qmdestroy:105:root@pam:")

	const exitStatus = "delete worker failed"

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/status", node), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{}}`)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/status/current", node, vmid), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":{"vmid":%d,"status":"stopped"}}`, vmid)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":{}}`)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d", node, vmid), func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodDelete, r.Method)
		fmt.Fprintf(w, `{"data":%q}`, upid)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/tasks/%s/status", node, upid), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":{"upid":%q,"node":%q,"type":"qmdestroy","id":"%d","user":"root@pam","status":"stopped","exitstatus":%q}}`,
			upid, node, vmid, exitStatus)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/tasks/%s/log", node, upid), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"n":1,"t":"delete worker failed"}]}`)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	waitInterval := 1
	waitTimeout := 5

	ig := &InstanceGroup{
		proxmox: proxmox.NewClient(server.URL),
		Settings: Settings{
			ProxmoxTaskWaitInterval: &waitInterval,
			ProxmoxTaskWaitTimeout:  &waitTimeout,
		},
		log: hclog.NewNullLogger(),
		// checkID only frees vmid, so a second Allocate can only succeed if
		// collectInstance wrongly released the reservation.
		vmids: newVMIDAllocator(func(_ context.Context, candidate int) (bool, error) {
			return candidate == vmid, nil
		}, func(context.Context) (int, error) {
			return vmid, nil
		}),
	}

	reserved, err := ig.vmids.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, vmid, reserved)

	ig.collectInstance(context.Background(), proxmox.ClusterResource{
		VMID: uint64(vmid),
		Node: node,
		Type: proxmoxResourceTypeQemu,
	})

	// The delete task failed, so the reservation must still be held.
	_, err = ig.vmids.Allocate(context.Background())
	require.ErrorIs(t, err, ErrVMIDAllocationFailed)
}
