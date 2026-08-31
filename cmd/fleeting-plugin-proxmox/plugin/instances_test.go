package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
)

func TestInstanceGroup_templateCloneOptions(t *testing.T) {
	type testCase struct {
		name                     string
		isTemplate               bool
		configuredStorage        string
		configuredBandwidthLimit *int
		expectedFull             bool
		expectedBWLimit          *uint64
		expectedErr              error
	}

	positiveLimit := 500
	zeroLimit := 0
	expectedLimit := uint64(500)

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
		{
			name:                     "Bandwidth limit unset",
			isTemplate:               true,
			configuredStorage:        "local",
			configuredBandwidthLimit: nil,
			expectedFull:             true,
			expectedBWLimit:          nil,
			expectedErr:              nil,
		},
		{
			name:                     "Bandwidth limit set",
			isTemplate:               true,
			configuredStorage:        "local",
			configuredBandwidthLimit: &positiveLimit,
			expectedFull:             true,
			expectedBWLimit:          &expectedLimit,
			expectedErr:              nil,
		},
		{
			name:                     "Bandwidth limit explicitly zero",
			isTemplate:               true,
			configuredStorage:        "local",
			configuredBandwidthLimit: &zeroLimit,
			expectedFull:             true,
			expectedBWLimit:          nil,
			expectedErr:              nil,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			template := proxmox.VirtualMachine{
				Template: proxmox.IsTemplate(testCase.isTemplate),
			}

			ig := InstanceGroup{
				Settings: Settings{
					Storage:             testCase.configuredStorage,
					CloneBandwidthLimit: testCase.configuredBandwidthLimit,
				},
			}

			result, err := ig.getTemplateCloneOptions(&template)
			require.ErrorIs(t, err, testCase.expectedErr)

			if err == nil {
				require.Equal(t, testCase.configuredStorage, result.Storage)
				require.Equal(t, testCase.expectedFull, bool(result.Full))
				require.Equal(t, testCase.expectedBWLimit, result.BWLimit)
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

// TestInstanceGroup_cloneConcurrencySemaphore_defaultsAndRespectsSetting covers
// the settings wiring: an InstanceGroup built directly (no Init /
// FillWithDefaults, as most tests in this package do) still gets a correctly
// sized, lazily-built semaphore rather than a nil channel that would block
// forever on the first send.
func TestInstanceGroup_cloneConcurrencySemaphore_defaultsAndRespectsSetting(t *testing.T) {
	unset := InstanceGroup{}
	require.Equal(t, DefaultCloneConcurrency, cap(unset.cloneConcurrencySemaphore()))

	concurrency := 2
	set := InstanceGroup{Settings: Settings{CloneConcurrency: &concurrency}}
	require.Equal(t, concurrency, cap(set.cloneConcurrencySemaphore()))

	// Repeated calls must reuse the same channel, not rebuild (and resize) it.
	first := set.cloneConcurrencySemaphore()
	second := set.cloneConcurrencySemaphore()
	require.True(t, first == second, "expected the semaphore channel to be built once and reused")
}

// TestInstanceGroup_cloneAndWaitForTemplate_boundsCloneConcurrency is a
// regression test: a counting stub spanning the real clone-and-wait
// call site (not just the semaphore helper in isolation) with an atomic
// high-water mark, run under -race. Both the clone POST handler and the task
// status handler increment/decrement while "active", matching exactly the span
// cloneAndWaitForTemplate holds the semaphore across - a semaphore that only
// wrapped the POST would let this test's high-water mark exceed
// clone_concurrency, since Proxmox's disk copy happens after the POST returns.
func TestInstanceGroup_cloneAndWaitForTemplate_boundsCloneConcurrency(t *testing.T) {
	const (
		node             = "pve-node"
		templateVMID     = 900
		cloneConcurrency = 2
		instanceCount    = 6
		handlerDelay     = 20 * time.Millisecond
	)

	var (
		active    atomic.Int64
		highWater atomic.Int64
	)

	bumpHighWater := func(cur int64) {
		for {
			hw := highWater.Load()
			if cur <= hw || highWater.CompareAndSwap(hw, cur) {
				return
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/qemu/%d/clone", node, templateVMID), func(w http.ResponseWriter, r *http.Request) {
		var cloneOptions proxmox.VirtualMachineCloneOptions
		require.NoError(t, json.NewDecoder(r.Body).Decode(&cloneOptions))

		bumpHighWater(active.Add(1))
		time.Sleep(handlerDelay)

		upid := fmt.Sprintf("UPID:%s:00001A2B:00000000:00000000:qmclone:%d:root@pam:", node, cloneOptions.NewID)
		fmt.Fprintf(w, `{"data":%q}`, upid)
	})
	mux.HandleFunc(fmt.Sprintf("/nodes/%s/tasks/", node), func(w http.ResponseWriter, r *http.Request) {
		upid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/nodes/%s/tasks/", node)), "/status")
		parts := strings.Split(upid, ":")

		time.Sleep(handlerDelay)
		active.Add(-1)

		fmt.Fprintf(w, `{"data":{"upid":%q,"node":%q,"type":"qmclone","id":%q,"user":"root@pam","status":"stopped","exitstatus":"OK"}}`,
			upid, node, parts[6])
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := proxmox.NewClient(server.URL)
	template := &proxmox.VirtualMachine{}
	template.New(client, node, templateVMID)

	waitInterval := 1
	waitTimeout := 5
	concurrency := cloneConcurrency

	ig := InstanceGroup{
		proxmox: client,
		Settings: Settings{
			Storage:                 "local",
			ProxmoxTaskWaitInterval: &waitInterval,
			ProxmoxTaskWaitTimeout:  &waitTimeout,
			CloneConcurrency:        &concurrency,
		},
		log: hclog.NewNullLogger(),
		// alwaysFreeCheckID + a constant nextID gives each of the 6 concurrent
		// Allocate calls a distinct vmid via local-advance (100..105).
		vmids: newVMIDAllocator(alwaysFreeCheckID, func(context.Context) (int, error) {
			return 100, nil
		}),
	}

	var wg sync.WaitGroup

	for range instanceCount {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if _, err := ig.cloneAndWaitForTemplate(context.Background(), template); err != nil {
				t.Errorf("cloneAndWaitForTemplate: %v", err)
			}
		}()
	}

	wg.Wait()

	require.LessOrEqual(t, highWater.Load(), int64(cloneConcurrency))
	require.Equal(t, int64(cloneConcurrency), highWater.Load(),
		"expected concurrency to actually reach clone_concurrency, not merely stay under it")
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
