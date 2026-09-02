package plugin

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// removalTestServer parameterises newRemovalTestGroup. The zero value is the default fake:
// one stale instance whose rename the API accepts and whose task then fails, and a logger
// that discards.
type removalTestServer struct {
	// log is the group's logger; nil means discard.
	log hclog.Logger

	// configResponse is the body the rename POST answers with; empty means a task UPID.
	configResponse string

	// requests tallies the requests the fake served; nil means do not report them.
	requests *removalRequestCounts
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

	configResponse := opts.configResponse
	if configResponse == "" {
		configResponse = fmt.Sprintf(`{"data":%q}`, renameUPID)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/pools"):
			fmt.Fprint(w, `{"data":[{"poolid":"test-pool","members":[{"vmid":100,"type":"qemu","name":"fleeting-creating","node":"pve-node"}]}]}`)
		case r.URL.Path == "/nodes/pve-node/status":
			fmt.Fprint(w, `{"data":{}}`)
		case r.URL.Path == "/nodes/pve-node/qemu/100/status/current":
			fmt.Fprint(w, `{"data":{"vmid":100,"name":"fleeting-creating","status":"running"}}`)
		case r.URL.Path == "/nodes/pve-node/qemu/100/config" && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"data":{}}`)
		case r.URL.Path == "/nodes/pve-node/qemu/100/config" && r.Method == http.MethodPost:
			counts.configPOSTs.Add(1)
			fmt.Fprint(w, configResponse)
		case strings.Contains(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
			counts.taskPolls.Add(1)
			fmt.Fprint(w, taskStatusBody("qmconfig", "stopped", "VM is locked (clone)"))
		case strings.HasSuffix(r.URL.Path, "/log"):
			fmt.Fprint(w, `{"data":[{"n":1,"t":"TASK ERROR: VM is locked (clone)"}]}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	waitInterval := 1
	templateID := 200

	return &InstanceGroup{
		Settings: Settings{
			Pool:                    "test-pool",
			TemplateID:              &templateID,
			InstanceNameCreating:    "fleeting-creating",
			InstanceNameRemoving:    "fleeting-removing",
			InstanceTagsRemoving:    "fleeting-removing",
			ProxmoxTaskWaitInterval: &waitInterval,
		},
		log:                       log,
		proxmox:                   proxmox.NewClient(server.URL),
		instanceCollectionTrigger: make(chan struct{}, 1),
	}
}

// Decrease's path: a rename the API accepted whose task then fails must surface as an error,
// so the instance is not reported as successfully removed.
func TestMarkInstancesForRemovalReportsTaskFailure(t *testing.T) {
	ig := newRemovalTestGroup(t, removalTestServer{})

	member := &proxmox.ClusterResource{VMID: 100, Type: vmTypeQEMU, Name: "fleeting-creating", Node: "pve-node"}

	err := ig.markInstancesForRemoval(context.Background(), member)
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

	err := ig.markInstancesForRemoval(context.Background(), member)
	require.ErrorIs(t, err, ErrNoTask)
	require.Equal(t, int64(1), counts.configPOSTs.Load(), "expected exactly one rename POST")
	require.Zero(t, counts.taskPolls.Load(), "expected the rename never to be waited on")
}
