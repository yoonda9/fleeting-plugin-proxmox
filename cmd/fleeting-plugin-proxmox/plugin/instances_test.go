package plugin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// newRemovalTestGroup wires an InstanceGroup to an httptest Proxmox holding one stale
// instance whose mark-for-removal rename is accepted by the API but whose task then fails.
func newRemovalTestGroup(t *testing.T) *InstanceGroup {
	t.Helper()

	renameUPID := testUPID("qmconfig")

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
			fmt.Fprintf(w, `{"data":%q}`, renameUPID)
		case strings.Contains(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/status"):
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
		log:                       hclog.NewNullLogger(),
		proxmox:                   proxmox.NewClient(server.URL),
		instanceCollectionTrigger: make(chan struct{}, 1),
	}
}

// Decrease's path: a rename the API accepted whose task then fails must surface as an error,
// so the instance is not reported as successfully removed.
func TestMarkInstancesForRemovalReportsTaskFailure(t *testing.T) {
	ig := newRemovalTestGroup(t)

	member := &proxmox.ClusterResource{VMID: 100, Type: vmTypeQEMU, Name: "fleeting-creating", Node: "pve-node"}

	err := ig.markInstancesForRemoval(context.Background(), member)
	require.ErrorIs(t, err, ErrTaskFailed)
}
