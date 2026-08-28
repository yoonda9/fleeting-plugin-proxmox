package plugin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
}

// newCollectTestGroup wires an InstanceGroup to an httptest Proxmox holding one stopped
// instance whose delete task finishes with exitStatus, and an allocator that has already
// reserved that instance's vmid, so a test can observe what collectInstance does with the
// reservation.
func newCollectTestGroup(t *testing.T, exitStatus string, checkID func(context.Context, int) (bool, error)) *InstanceGroup {
	t.Helper()

	const vmid = collectTestVMID

	mux := http.NewServeMux()
	// Stopped, so collectInstance skips the Stop call entirely.
	registerVMRoutes(mux, vmid, "stopped")
	mux.HandleFunc(fmt.Sprintf("DELETE /nodes/pve-node/qemu/%d", vmid),
		constantHandler(fmt.Sprintf(`{"data":%q}`, increaseTestUPID("qmdestroy", vmid))))
	mux.HandleFunc("GET /nodes/pve-node/tasks/", taskHandler(t, "qmdestroy", taskStatusStopped, exitStatus, exitStatus))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ig := newWaitTestGroup()
	ig.proxmox = proxmox.NewClient(server.URL)
	ig.vmids = newVMIDAllocator(checkID, constantNextID(vmid))

	reserved, err := ig.vmids.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, vmid, reserved)

	return ig
}

// collectTestVMID is the one instance newCollectTestGroup's fake serves.
const collectTestVMID = 105

// collectTestMember is that instance as the collector sees it in the pool listing.
func collectTestMember() proxmox.ClusterResource {
	return proxmox.ClusterResource{VMID: collectTestVMID, Node: "pve-node", Type: vmTypeQEMU}
}

// A confirmed delete is the release trigger the allocator's default mode rests on: a second
// Allocate must be able to reissue the vmid rather than treat it as still reserved.
func TestCollectInstanceReleasesVMIDAfterConfirmedDelete(t *testing.T) {
	ig := newCollectTestGroup(t, taskExitStatusOK, alwaysFreeCheckID)

	ig.collectInstance(context.Background(), collectTestMember())

	reallocated, err := ig.vmids.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, collectTestVMID, reallocated)
}

// A delete task that itself fails must leave the reservation in place instead of freeing an
// id that may still be live. checkID only frees the one vmid, so a second Allocate can only
// succeed if collectInstance wrongly released it.
func TestCollectInstanceKeepsVMIDWhenDeleteFails(t *testing.T) {
	ig := newCollectTestGroup(t, "delete worker failed", onlyFreeCheckID(collectTestVMID))

	ig.collectInstance(context.Background(), collectTestMember())

	_, err := ig.vmids.Allocate(context.Background())
	require.ErrorIs(t, err, ErrVMIDAllocationFailed)
}
