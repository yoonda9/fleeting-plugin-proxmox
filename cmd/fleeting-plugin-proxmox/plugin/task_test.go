package plugin

import (
	"context"
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

var testTaskUPID = testUPID("qmclone")

// testUPID builds the UPID of a task of the given type against the fake node.
func testUPID(taskType string) proxmox.UPID {
	return proxmox.UPID(fmt.Sprintf("UPID:pve-node:00001A2B:00000000:00000000:%s:100:root@pam:", taskType))
}

// taskStatusBody builds a /status response. It echoes back upid/node/type/id/user, matching
// real Proxmox responses: the vendored client's Task.Ping overwrites the whole struct from
// this payload on every poll, so omitting them would blank out task.UPID after the first poll
// and crash the second one.
func taskStatusBody(taskType, status, exitStatus string) string {
	return fmt.Sprintf(`{"data":{"upid":%q,"node":"pve-node","type":%q,"id":"100","user":"root@pam","status":%q,"exitstatus":%q}}`,
		testUPID(taskType), taskType, status, exitStatus)
}

// newWaitTestGroup is the minimum InstanceGroup waitTask needs: a poll interval and a logger.
func newWaitTestGroup() *InstanceGroup {
	waitInterval := 1

	return &InstanceGroup{
		Settings: Settings{ProxmoxTaskWaitInterval: &waitInterval},
		log:      hclog.NewNullLogger(),
	}
}

func TestClassifyTask(t *testing.T) {
	testCases := []struct {
		name        string
		status      string
		exitStatus  string
		expectedErr error
		expectText  string
	}{
		{
			name:       "success",
			status:     taskStatusStopped,
			exitStatus: taskExitStatusOK,
		},
		{
			// Proxmox omits the exit status for some task types.
			name:   "omitted exit status",
			status: taskStatusStopped,
		},
		{
			name:        "real failure",
			status:      taskStatusStopped,
			exitStatus:  "unable to parse volume ID 'local-lvm:'",
			expectedErr: ErrTaskFailed,
			expectText:  "unable to parse volume ID 'local-lvm:'",
		},
		{
			name:        "still running",
			status:      proxmox.TaskRunning,
			expectedErr: errStillRunning,
		},
		{
			// An unauthorized or empty /status response leaves the task struct blank, which
			// Task.Wait reports as no-longer-running. Its exit status is blank too, so falling
			// through to the exit status check would read a failed poll as success.
			name:        "blank status poll",
			expectedErr: ErrTaskFailed,
			expectText:  "never observed as stopped",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := classifyTask(testCase.status, testCase.exitStatus)

			if testCase.expectedErr == nil {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, testCase.expectedErr)

			if testCase.expectText != "" {
				require.Contains(t, err.Error(), testCase.expectText)
			}
		})
	}
}

// One end-to-end case for the wiring: classifyTask's verdict reaches the caller, and a real
// failure pulls Proxmox's own explanation into the log. The branch matrix is TestClassifyTask's.
func TestInstanceGroup_waitTask(t *testing.T) {
	var logFetched atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			fmt.Fprint(w, taskStatusBody("qmclone", taskStatusStopped, "unable to parse volume ID 'local-lvm:'"))
		case strings.HasSuffix(r.URL.Path, "/log"):
			logFetched.Store(true)
			fmt.Fprint(w, `{"data":[{"n":1,"t":"volume parse failed"}]}`)
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	task := proxmox.NewTask(testTaskUPID, proxmox.NewClient(server.URL))

	err := newWaitTestGroup().waitTask(context.Background(), task, time.Second)

	require.ErrorIs(t, err, ErrTaskFailed)
	require.Contains(t, err.Error(), "unable to parse volume ID 'local-lvm:'")
	require.True(t, logFetched.Load(), "expected waitTask to fetch the task log")
}

func TestInstanceGroup_waitTaskNilTask(t *testing.T) {
	// Some operations complete synchronously and yield no task: vm.ResizeDisk returns
	// (nil, nil) when Proxmox answers with null data.
	require.NoError(t, newWaitTestGroup().waitTask(context.Background(), nil, time.Second))
}
