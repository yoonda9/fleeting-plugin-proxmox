package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
		collectorShutdownTrigger:              make(chan struct{}),
		sessionTicketRefresherShutdownTrigger: make(chan struct{}),
	}

	ig.collectorWaitGroup.Go(func() {
		<-ig.collectorShutdownTrigger
	})

	ig.sessionTicketRefresherWaitGroup.Go(func() {
		<-ig.sessionTicketRefresherShutdownTrigger
	})

	// Shutdown must return every time, not just the first.
	for i := range 3 {
		done := make(chan error, 1)

		go func() {
			done <- ig.Shutdown(context.Background())
		}()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatalf("Shutdown call #%d did not return: possible deadlock", i+1)
		}
	}
}

func TestShutdownWithoutInitDoesNotPanic(t *testing.T) {
	// A group whose Init never ran still has nil trigger channels, and closing a nil channel
	// panics.
	ig := &InstanceGroup{}

	require.NotPanics(t, func() {
		require.NoError(t, ig.Shutdown(context.Background()))
	})
}

func TestShutdownAfterInitCycleDoesNotHang(t *testing.T) {
	ig := &InstanceGroup{}

	// A Shutdown before the first Init has nothing to close, and resetLifecycle re-arms the
	// guard on every Init, so what that first call does with the guard cannot strand a later
	// cycle. The requirement is only that every Shutdown here returns.
	requireShutdownReturns(t, ig, "before the first Init")

	for cycle := range 2 {
		ig.resetLifecycle()
		startFakeWorkers(ig)
		requireShutdownReturns(t, ig, fmt.Sprintf("after Init cycle #%d", cycle+1))
	}
}

// requireShutdownReturns fails the test instead of letting the package time out when Shutdown
// blocks -- a Shutdown that waits on workers nothing ever told to stop never returns at all.
func requireShutdownReturns(t *testing.T, ig *InstanceGroup, when string) {
	t.Helper()

	done := make(chan error, 1)

	go func() {
		done <- ig.Shutdown(context.Background())
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatalf("Shutdown %s did not return: possible deadlock", when)
	}
}

// startFakeWorkers stands in for the collector and the session ticket refresher: each returns
// once its shutdown trigger is closed, which is all that Shutdown's waits depend on.
func startFakeWorkers(ig *InstanceGroup) {
	ig.collectorWaitGroup.Go(func() {
		<-ig.collectorShutdownTrigger
	})

	ig.sessionTicketRefresherWaitGroup.Go(func() {
		<-ig.sessionTicketRefresherShutdownTrigger
	})
}

// Decrease counts instances that were already being removed as successes, but they are
// bystanders: nothing was attempted for them. A batch in which every rename Decrease actually
// attempted failed is still a failed batch, and a bystander must not report it as a success.
func TestDecreaseReportsOnlyAttemptedSuccesses(t *testing.T) {
	ig := newRemovalTestGroup(t, removalTestServer{members: []removalTestMember{
		{vmid: 100, name: "fleeting-running"},
		{vmid: 101, name: "fleeting-removing"},
	}})
	ig.InstanceNameRunning = "fleeting-running"

	// 100 is the only one attempted -- its rename is accepted and its task then fails -- while
	// 101 is counted without a request being made for it.
	succeeded, err := ig.Decrease(context.Background(), []string{"100", "101"})
	require.Equal(t, []string{"101"}, succeeded)
	require.ErrorIs(t, err, ErrTaskFailed)

	// Control: the same failing rename with no bystander to count. The error was always
	// reported here, so the bystander is the only difference between the two calls.
	succeeded, err = ig.Decrease(context.Background(), []string{"100"})
	require.Empty(t, succeeded)
	require.ErrorIs(t, err, ErrTaskFailed)
}

const (
	// increaseTestNode is the one node the Increase fake serves: it appears in the pool
	// listing, in every VM route and inside every task UPID.
	increaseTestNode = "pve-node"

	// increaseTestTemplateID is the VM the fake reports as a template, and so the only vmid
	// whose clone route exists.
	increaseTestTemplateID = 200
)

// increaseTestCloneVMIDs are the vmids /cluster/nextid hands out, in order. Increase
// serialises the nextid/clone pair under its clone mutex, so the first of these is always the
// one whose clone POST is refused and the second is always the one that deploys.
var increaseTestCloneVMIDs = []int{100, 101}

// increaseTestUPID builds the UPID of a task of the given type against a vmid on the fake
// node. Task.Ping reads the node, the type and the id back out of the UPID, so the tasks route
// can answer for any task the fake handed out without having recorded it.
func increaseTestUPID(taskType string, vmid int) string {
	return fmt.Sprintf("UPID:%s:00001A2B:00000000:00000000:%s:%d:root@pam:", increaseTestNode, taskType, vmid)
}

// constantHandler answers every request with body.
func constantHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }
}

// registerVMRoutes serves the three endpoints getProxmoxVM walks for a single VM on
// increaseTestNode: the node list entry, the VM's current status, and its config.
func registerVMRoutes(mux *http.ServeMux, vmid int, status string) {
	mux.HandleFunc("GET /nodes/"+increaseTestNode+"/status", constantHandler(`{"data":{}}`))
	mux.HandleFunc(fmt.Sprintf("GET /nodes/%s/qemu/%d/status/current", increaseTestNode, vmid),
		constantHandler(fmt.Sprintf(`{"data":{"vmid":%d,"status":%q}}`, vmid, status)))
	mux.HandleFunc(fmt.Sprintf("GET /nodes/%s/qemu/%d/config", increaseTestNode, vmid), constantHandler(`{"data":{}}`))
}

// newIncreaseTestGroup wires an InstanceGroup to an httptest Proxmox serving the whole deploy
// path -- nextid, clone, task, config, start, agent -- for the template and for the VMs cloned
// from it, and answers the FIRST clone POST with a 500. That is the smallest way to make one
// instance of a batch fail while the rest come up: Proxmox never accepted the clone, so there
// is no task and no VM behind it. A ServeMux rather than removalTestServer's switch because
// Increase drives eleven distinct routes and tells three of them apart only by method.
func newIncreaseTestGroup(t *testing.T, log hclog.Logger) *InstanceGroup {
	t.Helper()

	var (
		vmidsHandedOut atomic.Int64
		clonesSeen     atomic.Int64
	)

	poolMembers := []string{
		fmt.Sprintf(`{"vmid":%d,"type":"qemu","name":"template","node":%q}`, increaseTestTemplateID, increaseTestNode),
	}

	for _, vmid := range increaseTestCloneVMIDs {
		poolMembers = append(poolMembers,
			fmt.Sprintf(`{"vmid":%d,"type":"qemu","name":"fleeting-creating","node":%q}`, vmid, increaseTestNode))
	}

	poolBody := fmt.Sprintf(`{"data":[{"poolid":"test-pool","members":[%s]}]}`, strings.Join(poolMembers, ","))

	vmRoute := func(suffix string) string {
		return fmt.Sprintf("/nodes/%s/qemu/{vmid}/%s", increaseTestNode, suffix)
	}

	templateRoute := func(suffix string) string {
		return fmt.Sprintf("/nodes/%s/qemu/%d/%s", increaseTestNode, increaseTestTemplateID, suffix)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /pools/", constantHandler(poolBody))
	mux.HandleFunc("GET /cluster/status", constantHandler(`{"data":[]}`))
	mux.HandleFunc("GET /nodes/"+increaseTestNode+"/status", constantHandler(`{"data":{}}`))

	// The template is the one VM that must report itself as a template, or the clone is
	// refused before it reaches the API.
	mux.HandleFunc("GET "+templateRoute("status/current"),
		constantHandler(fmt.Sprintf(`{"data":{"vmid":%d,"name":"template","status":"stopped","template":1}}`, increaseTestTemplateID)))

	mux.HandleFunc("GET "+vmRoute("status/current"), func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"data":{"vmid":%s,"name":"fleeting-creating","status":"running"}}`, r.PathValue("vmid"))
	})

	mux.HandleFunc("GET "+vmRoute("config"), constantHandler(`{"data":{}}`))
	mux.HandleFunc("GET "+vmRoute("agent/get-osinfo"), constantHandler(`{"data":{"result":{}}}`))

	mux.HandleFunc("POST "+vmRoute("config"), func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"data":%q}`, increaseTestUPID("qmconfig", increaseTestVMID(t, r)))
	})

	mux.HandleFunc("POST "+vmRoute("status/start"), func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"data":%q}`, increaseTestUPID("qmstart", increaseTestVMID(t, r)))
	})

	mux.HandleFunc("GET /cluster/nextid", func(w http.ResponseWriter, _ *http.Request) {
		index := int(vmidsHandedOut.Add(1)) - 1
		if index >= len(increaseTestCloneVMIDs) {
			t.Errorf("fake was asked for more vmids than the %d it has", len(increaseTestCloneVMIDs))
			http.Error(w, "out of vmids", http.StatusInternalServerError)

			return
		}

		fmt.Fprintf(w, `{"data":"%d"}`, increaseTestCloneVMIDs[index])
	})

	mux.HandleFunc("POST "+templateRoute("clone"), func(w http.ResponseWriter, r *http.Request) {
		var cloneOptions proxmox.VirtualMachineCloneOptions
		if err := json.NewDecoder(r.Body).Decode(&cloneOptions); err != nil {
			t.Errorf("failed to decode clone options: %v", err)
			http.Error(w, "undecodable clone options", http.StatusBadRequest)

			return
		}

		if clonesSeen.Add(1) == 1 {
			http.Error(w, "clone refused", http.StatusInternalServerError)

			return
		}

		fmt.Fprintf(w, `{"data":%q}`, increaseTestUPID("qmclone", cloneOptions.NewID))
	})

	// Every task the fake hands out succeeded; the batch's one failure is the refused clone,
	// which never produced a task at all.
	mux.HandleFunc("GET /nodes/"+increaseTestNode+"/tasks/", func(w http.ResponseWriter, r *http.Request) {
		upid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/nodes/"+increaseTestNode+"/tasks/"), "/status")

		fields := strings.Split(upid, ":")
		if len(fields) < 8 {
			t.Errorf("task route asked for a malformed UPID: %q", upid)
			http.Error(w, "malformed upid", http.StatusBadRequest)

			return
		}

		fmt.Fprintf(w, `{"data":{"upid":%q,"node":%q,"type":%q,"id":%q,"user":"root@pam","status":%q,"exitstatus":%q}}`,
			upid, increaseTestNode, fields[5], fields[6], taskStatusStopped, taskExitStatusOK)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	templateID := increaseTestTemplateID

	ig := newWaitTestGroup()
	ig.Pool = "test-pool"
	ig.Storage = "local"
	ig.TemplateID = &templateID
	ig.InstanceNameCreating = "fleeting-creating"
	ig.InstanceNameRunning = "fleeting-running"
	ig.InstanceTagsCreating = "fleeting-creating"
	ig.InstanceTagsRunning = "fleeting-running"
	ig.log = log
	ig.proxmox = proxmox.NewClient(server.URL)

	return ig
}

// increaseTestVMID reads the vmid a VM route was called for, so a task UPID can name the VM
// whose task it is -- Task.Ping parses that id back out and the fake has to stay consistent
// with it.
func increaseTestVMID(t *testing.T, r *http.Request) int {
	t.Helper()

	vmid, err := strconv.Atoi(r.PathValue("vmid"))
	if err != nil {
		t.Errorf("VM route called with a non-numeric vmid %q: %v", r.PathValue("vmid"), err)
	}

	return vmid
}

// Increase must not report a partially successful batch as an error. fleeting's gRPC shim
// returns the response alongside the error and grpc-go discards the response whenever the
// error is non-nil, so the instances that did come up would reach the provisioner as no
// instances at all. What the nil error hides goes to the log instead. This is the only test
// that drives Increase at the RPC boundary.
func TestIncreaseReportsPartialBatch(t *testing.T) {
	log, logBuffer := newLogBuffer(t)
	ig := newIncreaseTestGroup(t, log)

	succeeded, err := ig.Increase(context.Background(), 2)

	require.NoError(t, err, "a partial success reported as an error reaches the provisioner as no instances at all")
	require.Equal(t, 1, succeeded, "the second clone deployed; the first was refused")
	require.Regexp(t, `\[WARN\].*failed to deploy some instances: attempted=2 failed=1`, logBuffer.String())
}

// Heartbeat's retry closure must hand classifyError the raw AgentOsInfo error: wrapping it
// first would defeat the 500/501 prefix check, and a real Proxmox 500 (a QMP get-osinfo timeout
// under load, say) would declare a healthy instance unhealthy on the first bad poll instead of
// retrying.
func TestHeartbeatToleratesTransientAgentBlip(t *testing.T) {
	t.Parallel()

	const (
		vmid = 100
		node = increaseTestNode
	)

	var agentRequests atomic.Int64

	mux := http.NewServeMux()

	mux.HandleFunc("GET /pools/",
		constantHandler(fmt.Sprintf(`{"data":[{"poolid":"test-pool","members":[{"id":"qemu/%d","type":"qemu","vmid":%d,"node":%q}]}]}`, vmid, vmid, node)))
	registerVMRoutes(mux, vmid, "running")

	mux.HandleFunc(fmt.Sprintf("GET /nodes/%s/qemu/%d/agent/get-osinfo", node, vmid), func(w http.ResponseWriter, r *http.Request) {
		if agentRequests.Add(1) <= 2 {
			// A bare 500 with a *valid* JSON body: go-proxmox's handleResponse returns before
			// reading the body for 500/501, so classTransient must come from the status
			// prefix, not an incidental decode failure.
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"data":{}}`)

			return
		}

		agentOsInfoSuccessHandler(w, r)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	retryAttempts := 3

	ig := newWaitTestGroup()
	ig.Pool = "test-pool"
	ig.ProxmoxAPIRetryAttempts = &retryAttempts
	ig.proxmox = proxmox.NewClient(server.URL)

	require.NoError(t, ig.Heartbeat(context.Background(), strconv.Itoa(vmid)))
	require.Equal(t, int64(3), agentRequests.Load(), "the two transient polls must be retried, not reported")
}
