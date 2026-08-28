package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
)

func TestInstanceGroup_templateCloneOptions(t *testing.T) {
	type testCase struct {
		name              string
		isTemplate        bool
		configuredStorage string
		expectedFull      bool
		expectedErr       error
	}

	testCases := []testCase{
		{
			name:              "VM with unconfigured storage", // Error?
			isTemplate:        false,
			configuredStorage: "",
			expectedFull:      true,
			expectedErr:       ErrCloneVMWithoutConfiguredStorage,
		},
		{
			name:              "VM with configured storage",
			isTemplate:        false,
			configuredStorage: "local",
			expectedFull:      true,
			expectedErr:       nil,
		},
		{
			name:              "Template with unconfigured storage",
			isTemplate:        true,
			configuredStorage: "",
			expectedFull:      false,
			expectedErr:       nil,
		},
		{
			name:              "Template with configured storage",
			isTemplate:        true,
			configuredStorage: "local",
			expectedFull:      true,
			expectedErr:       nil,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			template := proxmox.VirtualMachine{
				Template: proxmox.IsTemplate(testCase.isTemplate),
			}

			ig := InstanceGroup{
				Settings: Settings{
					Storage: testCase.configuredStorage,
				},
			}

			result, err := ig.getTemplateCloneOptions(&template)
			require.ErrorIs(t, err, testCase.expectedErr)

			if err == nil {
				require.Equal(t, testCase.configuredStorage, result.Storage)
				require.Equal(t, testCase.expectedFull, bool(result.Full))
			}
		})
	}
}

// TestInstanceGroup_cloneTemplate_passesAllocatedVMID is a regression test:
// getTemplateCloneOptions alone does not prove vmidAllocator's result actually
// reaches the clone POST as an explicit NewID - that wiring lives in
// cloneTemplate.
func TestInstanceGroup_cloneTemplate_passesAllocatedVMID(t *testing.T) {
	const node = "pve-node"

	const templateVMID = 900

	const allocatedVMID = 105

	var cloneOptions proxmox.VirtualMachineCloneOptions

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/clone", node, templateVMID), func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&cloneOptions))

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":"UPID:pve-node:00001A2B:00000000:00000000:qmclone:105:root@pam:"}`)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := proxmox.NewClient(server.URL)
	template := &proxmox.VirtualMachine{}
	template.New(client, node, templateVMID)

	ig := InstanceGroup{
		proxmox: client,
		Settings: Settings{
			Storage: "local",
		},
		// Only allocatedVMID is ever free: Allocate's local-advance would
		// otherwise walk past a merely-reserved id onto a free neighbour, which
		// would defeat the "still reserved" assertion below.
		vmids: newVMIDAllocator(func(_ context.Context, vmid int) (bool, error) {
			return vmid == allocatedVMID, nil
		}, func(context.Context) (int, error) {
			return allocatedVMID, nil
		}),
	}

	vmid, task, err := ig.cloneTemplate(context.Background(), template)

	require.NoError(t, err)
	require.Equal(t, allocatedVMID, vmid)
	require.NotNil(t, task)
	require.Equal(t, allocatedVMID, cloneOptions.NewID)

	// The vmid must still be reserved: a second Allocate call must not reissue it.
	_, err = ig.vmids.Allocate(context.Background())
	require.ErrorIs(t, err, ErrVMIDAllocationFailed)
}

// TestInstanceGroup_cloneTemplate_releasesVMIDOnCloneFailure proves the other
// documented release trigger for cloneTemplate: a failed clone POST must give
// the vmid back so it is not leaked as permanently unallocatable.
func TestInstanceGroup_cloneTemplate_releasesVMIDOnCloneFailure(t *testing.T) {
	const node = "pve-node"

	const templateVMID = 900

	const allocatedVMID = 105

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/clone", node, templateVMID), func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "clone failed", http.StatusInternalServerError)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := proxmox.NewClient(server.URL)
	template := &proxmox.VirtualMachine{}
	template.New(client, node, templateVMID)

	ig := InstanceGroup{
		proxmox: client,
		Settings: Settings{
			Storage: "local",
		},
		vmids: newVMIDAllocator(alwaysFreeCheckID, func(context.Context) (int, error) {
			return allocatedVMID, nil
		}),
	}

	_, _, err := ig.cloneTemplate(context.Background(), template)
	require.Error(t, err)

	// A failed clone POST must have released the vmid, not leaked it.
	vmid, err := ig.vmids.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, allocatedVMID, vmid)
}

// TestInstanceGroup_deployInstance_releasesVMIDWhenCloneTaskFails is a
// regression test: the clone POST can succeed while the clone worker itself
// fails, and that path released nothing, so the vmid stayed reserved forever
// - Proxmox rolls the target config back, so /cluster/nextid keeps proposing
// that same id and every later clone in the process burns all of
// vmidAllocateAttempts on it.
func TestInstanceGroup_deployInstance_releasesVMIDWhenCloneTaskFails(t *testing.T) {
	const node = "pve-node"

	const templateVMID = 900

	const allocatedVMID = 105

	const upid = proxmox.UPID("UPID:pve-node:00001A2B:00000000:00000000:qmclone:105:root@pam:")

	const exitStatus = "clone worker failed"

	var checkIDCalls atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/clone", node, templateVMID), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":%q}`, upid)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/tasks/%s/status", node, upid), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":{"upid":%q,"node":%q,"type":"qmclone","id":"%d","user":"root@pam","status":"stopped","exitstatus":%q}}`,
			upid, node, allocatedVMID, exitStatus)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/tasks/%s/log", node, upid), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"n":1,"t":"clone worker failed"}]}`)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := proxmox.NewClient(server.URL)
	template := &proxmox.VirtualMachine{}
	template.New(client, node, templateVMID)

	waitInterval := 1
	waitTimeout := 5

	ig := InstanceGroup{
		proxmox: client,
		Settings: Settings{
			Storage:                 "local",
			ProxmoxTaskWaitInterval: &waitInterval,
			ProxmoxTaskWaitTimeout:  &waitTimeout,
		},
		log: hclog.NewNullLogger(),
		// checkID only frees allocatedVMID, so a second Allocate can only
		// succeed here if ReleaseIfFree actually ran.
		vmids: newVMIDAllocator(func(_ context.Context, vmid int) (bool, error) {
			checkIDCalls.Add(1)

			return vmid == allocatedVMID, nil
		}, func(context.Context) (int, error) {
			return allocatedVMID, nil
		}),
	}

	_, err := ig.deployInstance(context.Background(), template)

	require.ErrorIs(t, err, ErrTaskFailed)

	require.Positive(t, checkIDCalls.Load(), "expected deployInstance to consult the cluster before releasing")

	reallocated, err := ig.vmids.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, allocatedVMID, reallocated)
}
