package plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/luthermonson/go-proxmox"
)

const taskLogLineLimit = 50

var (
	ErrTaskFailed       = errors.New("proxmox task failed")
	errTaskStillRunning = errors.New("proxmox task is still running")
)

// classifyTask decides a task's outcome from what Task.Ping already populated.
// Pure: no receiver, no logging, no I/O.
func classifyTask(status, exitStatus string) error {
	if status == proxmox.TaskRunning {
		return errTaskStillRunning
	}

	// Proxmox omits the exit status for some task types; absent means success.
	if exitStatus == "" || exitStatus == "OK" {
		return nil
	}

	return fmt.Errorf("%w: %s", ErrTaskFailed, exitStatus)
}

// timeout is a parameter, not a constant, because instances.go and collector.go pass different
// timeouts (proxmoxTaskWaitTimeout vs collectionTimeout) even though both happen to be 5 minutes today.
func (ig *InstanceGroup) waitTask(ctx context.Context, task *proxmox.Task, timeout time.Duration) error {
	interval := time.Duration(*ig.ProxmoxTaskWaitInterval) * time.Second

	err := task.Wait(ctx, interval, timeout)
	if err != nil {
		return fmt.Errorf("failed while waiting for task '%s': %w", task.UPID, err)
	}

	err = classifyTask(task.Status, task.ExitStatus)
	if err != nil {
		ig.logTaskFailure(ctx, task)
		return err
	}

	return nil
}

// logTaskFailure is best effort: a failure to fetch the log must never replace the task error.
func (ig *InstanceGroup) logTaskFailure(ctx context.Context, task *proxmox.Task) {
	log, err := task.Log(ctx, 0, taskLogLineLimit)
	if err != nil {
		ig.log.Error("Failed to fetch log for failed task", "upid", task.UPID, "exitstatus", task.ExitStatus, "err", err)
		return
	}

	ig.log.Error("Proxmox task failed", "upid", task.UPID, "exitstatus", task.ExitStatus, "log", log)
}
