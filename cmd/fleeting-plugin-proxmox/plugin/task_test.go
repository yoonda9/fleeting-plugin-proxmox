package plugin

import (
	"context"
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

func TestClassifyTask(t *testing.T) {
	type testCase struct {
		name       string
		status     string
		exitStatus string
		expectErr  bool
		expectFail bool
	}

	testCases := []testCase{
		{
			name:       "success",
			status:     "stopped",
			exitStatus: "OK",
			expectErr:  false,
		},
		{
			name:       "real failure",
			status:     "stopped",
			exitStatus: "unable to parse volume ID 'local-lvm:'",
			expectErr:  true,
			expectFail: true,
		},
		{
			name:       "still running",
			status:     "running",
			exitStatus: "",
			expectErr:  true,
			expectFail: false,
		},
		{
			name:       "omitted exit status",
			status:     "stopped",
			exitStatus: "",
			expectErr:  false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := classifyTask(testCase.status, testCase.exitStatus)

			if !testCase.expectErr {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			require.Equal(t, testCase.expectFail, errors.Is(err, ErrTaskFailed))

			if testCase.expectFail {
				require.Contains(t, err.Error(), testCase.exitStatus)
			}
		})
	}
}

func TestInstanceGroup_waitTask_failurePath(t *testing.T) {
	const upid = proxmox.UPID("UPID:pve-node:00001A2B:00000000:00000000:qmclone:100:root@pam:")
	const exitStatus = "unable to parse volume ID 'local-lvm:'"

	var logFetched atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			// Echo back upid/node/type/id/user, matching real Proxmox responses: the
			// vendored client's Task.Ping overwrites the whole struct from this
			// payload on every poll, so omitting them would blank out task.UPID
			// after the first poll and crash the second one.
			fmt.Fprintf(w, `{"data":{"upid":%q,"node":"pve-node","type":"qmclone","id":"100","user":"root@pam","status":"stopped","exitstatus":%q}}`, upid, exitStatus)
		case strings.HasSuffix(r.URL.Path, "/log"):
			logFetched.Store(true)
			fmt.Fprint(w, `{"data":[{"n":1,"t":"volume parse failed"}]}`)
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	task := proxmox.NewTask(upid, proxmox.NewClient(server.URL))

	waitInterval := 1
	ig := InstanceGroup{
		Settings: Settings{ProxmoxTaskWaitInterval: &waitInterval},
		log:      hclog.NewNullLogger(),
	}

	err := ig.waitTask(context.Background(), task, time.Second)

	require.ErrorIs(t, err, ErrTaskFailed)
	require.Contains(t, err.Error(), exitStatus)
	require.True(t, logFetched.Load(), "expected waitTask to fetch the task log on failure")
}
