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

func TestInstanceGroup_waitTask_transientThenSuccess(t *testing.T) {
	const upid = proxmox.UPID("UPID:pve-node:00001A2B:00000000:00000000:qmclone:100:root@pam:")

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)

			return
		}

		// The first two polls simulate an overloaded pveproxy: a 502 with a
		// non-JSON (HTML) body, which surfaces to the plugin as a JSON decode
		// error rather than a typed status - classifyError must still treat it
		// as transient so the task wait keeps polling instead of failing.
		if requests.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>Bad Gateway</html>"))

			return
		}

		fmt.Fprintf(w, `{"data":{"upid":%q,"node":"pve-node","type":"qmclone","id":"100","user":"root@pam","status":"stopped","exitstatus":"OK"}}`, upid)
	}))
	defer server.Close()

	task := proxmox.NewTask(upid, proxmox.NewClient(server.URL))

	waitInterval := 1
	ig := InstanceGroup{
		Settings: Settings{ProxmoxTaskWaitInterval: &waitInterval},
		log:      hclog.NewNullLogger(),
	}

	err := ig.waitTask(context.Background(), task, 5*time.Second)

	require.NoError(t, err)
	require.GreaterOrEqual(t, requests.Load(), int64(3))
}

func TestInstanceGroup_waitTask_transientOn500ThenSuccess(t *testing.T) {
	const upid = proxmox.UPID("UPID:pve-node:00001A2B:00000000:00000000:qmclone:100:root@pam:")

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)

			return
		}

		// A bare 500 - go-proxmox's handleResponse returns errors.New(res.Status)
		// for it without ever reading the body, so this is a genuine status-based
		// classification, not a coincidence of an unparsable body like the
		// HTML/502 case above. A valid JSON body proves that: if classifyError
		// were instead relying on a decode failure, this case would fail to
		// classify as transient and waitTask would give up here.
		if requests.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"data":{"result":"ok"}}`))

			return
		}

		fmt.Fprintf(w, `{"data":{"upid":%q,"node":"pve-node","type":"qmclone","id":"100","user":"root@pam","status":"stopped","exitstatus":"OK"}}`, upid)
	}))
	defer server.Close()

	task := proxmox.NewTask(upid, proxmox.NewClient(server.URL))

	waitInterval := 1
	ig := InstanceGroup{
		Settings: Settings{ProxmoxTaskWaitInterval: &waitInterval},
		log:      hclog.NewNullLogger(),
	}

	err := ig.waitTask(context.Background(), task, 5*time.Second)

	require.NoError(t, err)
	require.GreaterOrEqual(t, requests.Load(), int64(3))
}
