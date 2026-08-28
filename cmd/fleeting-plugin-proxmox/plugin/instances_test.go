package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// removalTestServer parameterises newRemovalTestGroup. The zero value is the default fake:
// one stale instance whose rename the API accepts and whose task then fails, and a logger
// that discards.
type removalTestServer struct {
	// log is the group's logger; nil means discard.
	log hclog.Logger

	// configResponse is the body the rename POST answers with; empty means a task UPID.
	configResponse string

	// members is the pool the fake serves; empty means the one stale instance above.
	members []removalTestMember

	// requests tallies the requests the fake served; nil means do not report them.
	requests *removalRequestCounts
}

// removalTestMember is one VM in the fake's pool. It appears in the pool listing and answers a
// status and a config fetch under its own vmid, which is everything a rename needs.
type removalTestMember struct {
	vmid uint64
	name string
}

// removalRequestCounts records how often the fake was asked for each of the two things a
// rename can do, so a test can assert on what was *not* requested. The counters are atomic
// because markInstancesForRemoval drives the fake from one goroutine per instance.
type removalRequestCounts struct {
	configPOSTs atomic.Int64
	taskPolls   atomic.Int64
}

// newLogBuffer returns a logger and the buffer it writes to, so a test can assert on what a
// call logged rather than only on what it returned.
func newLogBuffer(t *testing.T) (hclog.Logger, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}

	return hclog.New(&hclog.LoggerOptions{Output: buf}), buf
}

// newRemovalTestGroup wires an InstanceGroup to an httptest Proxmox holding one stale
// instance whose mark-for-removal rename is accepted by the API but whose task then fails.
func newRemovalTestGroup(t *testing.T, opts removalTestServer) *InstanceGroup {
	t.Helper()

	log := opts.log
	if log == nil {
		log = hclog.NewNullLogger()
	}

	counts := opts.requests
	if counts == nil {
		counts = &removalRequestCounts{}
	}

	renameUPID := testUPID("qmconfig")
	renameTask := taskHandler(t, "qmconfig", taskStatusStopped, "VM is locked (clone)", "TASK ERROR: VM is locked (clone)")

	configResponse := opts.configResponse
	if configResponse == "" {
		configResponse = fmt.Sprintf(`{"data":%q}`, renameUPID)
	}

	members := opts.members
	if len(members) == 0 {
		members = []removalTestMember{{vmid: 100, name: "fleeting-creating"}}
	}

	var (
		poolMembers  = make([]string, 0, len(members))
		memberStatus = make(map[string]string, len(members))
		memberConfig = make(map[string]bool, len(members))
	)

	for _, member := range members {
		poolMembers = append(poolMembers,
			fmt.Sprintf(`{"vmid":%d,"type":"qemu","name":%q,"node":"pve-node"}`, member.vmid, member.name))
		memberStatus[fmt.Sprintf("/nodes/pve-node/qemu/%d/status/current", member.vmid)] =
			fmt.Sprintf(`{"data":{"vmid":%d,"name":%q,"status":"running"}}`, member.vmid, member.name)
		memberConfig[fmt.Sprintf("/nodes/pve-node/qemu/%d/config", member.vmid)] = true
	}

	poolBody := fmt.Sprintf(`{"data":[{"poolid":"test-pool","members":[%s]}]}`, strings.Join(poolMembers, ","))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/pools"):
			fmt.Fprint(w, poolBody)
		case r.URL.Path == "/nodes/pve-node/status":
			fmt.Fprint(w, `{"data":{}}`)
		case memberStatus[r.URL.Path] != "":
			fmt.Fprint(w, memberStatus[r.URL.Path])
		case memberConfig[r.URL.Path] && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"data":{}}`)
		case memberConfig[r.URL.Path] && r.Method == http.MethodPost:
			counts.configPOSTs.Add(1)
			fmt.Fprint(w, configResponse)
		case strings.Contains(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			counts.taskPolls.Add(1)
			renameTask(w, r)
		default:
			renameTask(w, r)
		}
	}))
	t.Cleanup(server.Close)

	templateID := 200

	ig := newWaitTestGroup()
	ig.Pool = "test-pool"
	ig.TemplateID = &templateID
	ig.InstanceNameCreating = "fleeting-creating"
	ig.InstanceNameRemoving = "fleeting-removing"
	ig.InstanceTagsRemoving = "fleeting-removing"
	ig.log = log
	ig.proxmox = proxmox.NewClient(server.URL)
	ig.instanceCollectionTrigger = make(chan struct{}, 1)

	return ig
}

// Decrease's path: a rename the API accepted whose task then fails must surface as an error,
// so the instance is not reported as successfully removed.
func TestMarkInstanceForRemovalReportsTaskFailure(t *testing.T) {
	ig := newRemovalTestGroup(t, removalTestServer{})

	member := &proxmox.ClusterResource{VMID: 100, Type: vmTypeQEMU, Name: "fleeting-creating", Node: "pve-node"}

	err := ig.markInstanceForRemoval(context.Background(), member)
	require.ErrorIs(t, err, ErrTaskFailed)
}

// Init's stale sweep is cleanup, not a precondition for serving: a leftover VM that cannot be
// marked for removal must not stop the plugin from starting, and the failure must be logged
// rather than swallowed.
func TestMarkStaleInstancesForRemovalToleratesTaskFailure(t *testing.T) {
	log, logBuffer := newLogBuffer(t)
	ig := newRemovalTestGroup(t, removalTestServer{log: log})

	err := ig.markStaleInstancesForRemoval(context.Background())
	require.NoError(t, err)
	require.Regexp(t, `\[ERROR\].*continuing startup`, logBuffer.String())
}

// The rename is the only evidence the collector will ever see that an instance is to be
// removed. A rename Proxmox accepts without answering with a task was never observed, so it
// must be reported as an error rather than as a completed removal.
func TestMarkInstanceForRemovalRejectsMissingTask(t *testing.T) {
	counts := &removalRequestCounts{}
	ig := newRemovalTestGroup(t, removalTestServer{
		configResponse: `{"data":null}`,
		requests:       counts,
	})

	member := &proxmox.ClusterResource{VMID: 100, Type: vmTypeQEMU, Name: "fleeting-creating", Node: "pve-node"}

	err := ig.markInstanceForRemoval(context.Background(), member)
	require.ErrorIs(t, err, ErrNoTask)
	require.Equal(t, int64(1), counts.configPOSTs.Load(), "expected exactly one rename POST")
	require.Zero(t, counts.taskPolls.Load(), "expected the rename never to be waited on")
}

// newCloneTestGroup wires an InstanceGroup to an httptest Proxmox whose one route is the
// template's clone endpoint, served by handler, with an allocator that only ever offers
// cloneTestVMID: Allocate's local advance would otherwise walk past a merely-reserved id onto
// a free neighbour, which would defeat a "still reserved" assertion. checkID is called for
// every candidate the allocator considers, including ReleaseIfFree's reconfirmation.
func newCloneTestGroup(t *testing.T, handler http.HandlerFunc) (*InstanceGroup, *proxmox.VirtualMachine, *atomic.Int64) {
	t.Helper()

	checkIDCalls := new(atomic.Int64)

	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("POST /nodes/pve-node/qemu/%d/clone", cloneTestTemplateID), handler)
	mux.HandleFunc("GET /nodes/pve-node/tasks/", taskHandler(t, "qmclone", taskStatusStopped, "clone worker failed", "clone worker failed"))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ig := newWaitTestGroup()
	ig.Storage = "local"
	ig.proxmox = proxmox.NewClient(server.URL)
	ig.vmids = newVMIDAllocator(func(_ context.Context, vmid int) (bool, error) {
		checkIDCalls.Add(1)

		return vmid == cloneTestVMID, nil
	}, constantNextID(cloneTestVMID))

	return ig, newTestVM(ig.proxmox, cloneTestTemplateID), checkIDCalls
}

const (
	cloneTestTemplateID = 900
	cloneTestVMID       = 105
)

// getTemplateCloneOptions alone does not prove the allocator's result reaches the clone POST
// as an explicit NewID -- that wiring lives in cloneTemplate.
func TestCloneTemplatePassesAllocatedVMID(t *testing.T) {
	var cloneOptions proxmox.VirtualMachineCloneOptions

	ig, template, _ := newCloneTestGroup(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&cloneOptions))

		fmt.Fprintf(w, `{"data":%q}`, testUPID("qmclone"))
	})

	vmid, task, err := ig.cloneTemplate(context.Background(), template)

	require.NoError(t, err)
	require.Equal(t, cloneTestVMID, vmid)
	require.NotNil(t, task)
	require.Equal(t, cloneTestVMID, cloneOptions.NewID)

	// The vmid must still be reserved: a second Allocate call must not reissue it.
	_, err = ig.vmids.Allocate(context.Background())
	require.ErrorIs(t, err, ErrVMIDAllocationFailed)
}

// A failed clone POST must give the vmid back, or it is leaked as permanently unallocatable.
func TestCloneTemplateReleasesVMIDOnCloneFailure(t *testing.T) {
	ig, template, _ := newCloneTestGroup(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "clone failed", http.StatusInternalServerError)
	})

	_, _, err := ig.cloneTemplate(context.Background(), template)
	require.Error(t, err)

	vmid, err := ig.vmids.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, cloneTestVMID, vmid)
}

// The clone POST can succeed while the clone worker itself fails. Proxmox rolls the target
// config back, so /cluster/nextid keeps proposing the same id; were it left reserved, every
// later clone in the process would burn all of vmidAllocateAttempts on it.
func TestDeployInstanceReleasesVMIDWhenCloneTaskFails(t *testing.T) {
	ig, template, checkIDCalls := newCloneTestGroup(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":%q}`, testUPID("qmclone"))
	})

	_, err := ig.deployInstance(context.Background(), template)
	require.ErrorIs(t, err, ErrTaskFailed)

	// checkID only frees cloneTestVMID, so a second Allocate can only succeed here if
	// ReleaseIfFree actually asked the cluster before releasing.
	allocateChecks := checkIDCalls.Load()
	require.Positive(t, allocateChecks, "expected deployInstance to consult the cluster before releasing")

	reallocated, err := ig.vmids.Allocate(context.Background())
	require.NoError(t, err)
	require.Equal(t, cloneTestVMID, reallocated)
}

// A counting fake spanning the real clone-and-wait call site, with an atomic high-water mark,
// run under -race. Both the clone POST and the task status handler count as "active", matching
// exactly the span cloneAndWaitForTemplate holds the semaphore across: a semaphore that only
// wrapped the POST would let the high-water mark exceed clone_concurrency, since Proxmox's
// disk copy happens after the POST returns.
func TestCloneAndWaitForTemplateBoundsCloneConcurrency(t *testing.T) {
	const (
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
	mux.HandleFunc(fmt.Sprintf("POST /nodes/pve-node/qemu/%d/clone", cloneTestTemplateID), func(w http.ResponseWriter, r *http.Request) {
		var cloneOptions proxmox.VirtualMachineCloneOptions
		require.NoError(t, json.NewDecoder(r.Body).Decode(&cloneOptions))

		bumpHighWater(active.Add(1))
		time.Sleep(handlerDelay)

		fmt.Fprintf(w, `{"data":%q}`, increaseTestUPID("qmclone", cloneOptions.NewID))
	})
	taskSucceeded := taskSucceededHandler(t)

	mux.HandleFunc("GET /nodes/pve-node/tasks/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(handlerDelay)
		active.Add(-1)

		taskSucceeded(w, r)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	ig := newWaitTestGroup()
	ig.Storage = "local"
	ig.proxmox = proxmox.NewClient(server.URL)
	ig.cloneSemaphore = make(chan struct{}, cloneConcurrency)
	// alwaysFreeCheckID and a constant nextID give each of the concurrent Allocate calls a
	// distinct vmid through the allocator's local advance.
	ig.vmids = newVMIDAllocator(alwaysFreeCheckID, constantNextID(100))

	template := newTestVM(ig.proxmox, cloneTestTemplateID)

	errs := runParallel(instanceCount, func(int) error {
		_, err := ig.cloneAndWaitForTemplate(context.Background(), template)

		return err
	})
	require.NoError(t, errors.Join(errs...))

	require.Equal(t, int64(cloneConcurrency), highWater.Load(),
		"expected concurrency to reach clone_concurrency exactly: neither exceed it nor stay under it")
}
