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

// taskWaitTimeout returns the configured Proxmox task wait timeout as a Duration.
func (ig *InstanceGroup) taskWaitTimeout() time.Duration {
	return time.Duration(*ig.ProxmoxTaskWaitTimeout) * time.Second
}

// waitTask polls task itself rather than delegating to Task.Wait, which returns on
// the very first Ping error (vendor tasks.go:160-162). Task waits are where the
// plugin spends nearly all of its time, so that is where a loaded node is most
// likely to blip: a single 502 from an overloaded pveproxy would otherwise fail a
// five-minute clone that is progressing perfectly well. classTransient Ping errors
// are treated as "keep waiting"; anything else fails the wait immediately.
func (ig *InstanceGroup) waitTask(ctx context.Context, task *proxmox.Task, timeout time.Duration) error {
	interval := time.Duration(*ig.ProxmoxTaskWaitInterval) * time.Second

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		pingErr := task.Ping(ctx)

		switch {
		case pingErr == nil && task.Status != proxmox.TaskRunning:
			err := classifyTask(task.Status, task.ExitStatus)
			if err != nil {
				ig.logTaskFailure(ctx, task)
			}

			return err
		case pingErr != nil && ctx.Err() == nil && classifyError(pingErr) == classTransient:
			ig.log.Warn("transient error polling proxmox task, will keep waiting", "upid", task.UPID, "err", pingErr)
		case pingErr != nil:
			return fmt.Errorf("failed while waiting for task '%s': %w", task.UPID, pingErr)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("failed while waiting for task '%s': %w", task.UPID, ctx.Err())
		case <-ticker.C:
		}
	}
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
