package plugin

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/luthermonson/go-proxmox"
)

const (
	// taskStatusStopped is the status Proxmox reports for a task that has finished.
	taskStatusStopped = "stopped"

	// taskExitStatusOK is the exit status Proxmox reports for a task that succeeded.
	taskExitStatusOK = "OK"

	// taskLogLineLimit is the most log lines ever fetched for a failure log entry.
	taskLogLineLimit = 50
)

var (
	ErrTaskFailed = errors.New("proxmox task failed")

	// ErrNoTask reports an operation Proxmox accepted without returning a task to wait on, so
	// its outcome was never observed.
	ErrNoTask = errors.New("proxmox returned no task")
)

// classifyTask decides a finished task's outcome from what Task.Ping already populated. It is
// pure so the branch matrix is table-testable without an HTTP server.
func classifyTask(status, exitStatus string) error {
	// Task.Wait returns nil whenever the reported status is anything but running -- including
	// when a failed or unauthorized /status poll leaves the whole struct blank, which arrives
	// here as an empty status. Only a task actually observed as stopped can be trusted, so
	// this deliberately does NOT fall through to the exit status check below: a blank poll has
	// an empty exit status too, which would otherwise read as success.
	if status != taskStatusStopped {
		return fmt.Errorf("%w: never observed as stopped (status '%s')", ErrTaskFailed, status)
	}

	// Proxmox omits the exit status for some task types; on a stopped task, absent means
	// success -- which is why this is not task.IsSuccessful, as the client reports an omitted
	// exit status as failed.
	if exitStatus == "" || exitStatus == taskExitStatusOK {
		return nil
	}

	return fmt.Errorf("%w: %s", ErrTaskFailed, exitStatus)
}

// waitTask waits for a Proxmox task to finish and turns a non-OK exit status into an error.
//
// It polls the task itself rather than delegating to Task.Wait, which returns on the very
// first Ping error. Task waits are where the plugin spends nearly all of its time, so that is
// where a loaded node is most likely to blip: a single 502 from an overloaded pveproxy would
// otherwise fail a five-minute clone that is progressing perfectly well. A transient Ping
// error is logged and the wait carries on; anything else fails the wait immediately.
func (ig *InstanceGroup) waitTask(ctx context.Context, task *proxmox.Task) error {
	// go-proxmox's NewTask returns nil for an empty UPID, so an operation Proxmox answers with
	// null data yields no task. That may be a synchronous completion, so it is not an error
	// here -- but it is never silent, and a caller for which "no task" means "not verified"
	// checks for nil itself before calling this.
	if task == nil {
		ig.log.Warn("Proxmox returned no task to wait on, treating the operation as complete")

		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, seconds(ig.ProxmoxTaskWaitTimeout))
	defer cancel()

	// NewTicker panics on a non-positive interval, which is why proxmox_task_wait_interval
	// is validated positive rather than left to the operator.
	ticker := time.NewTicker(seconds(ig.ProxmoxTaskWaitInterval))
	defer ticker.Stop()

	for {
		pingErr := task.Ping(ctx)

		switch {
		case pingErr == nil && task.Status != proxmox.TaskRunning:
			return ig.taskOutcome(ctx, task)
		case pingErr != nil && ctx.Err() == nil && classifyError(pingErr) == classTransient:
			ig.log.Warn("Transient error polling Proxmox task, will keep waiting", "upid", task.UPID, "err", pingErr)
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

// taskOutcome turns a task that is no longer running into classifyTask's verdict, and pulls
// Proxmox's own explanation into the log for a real failure.
func (ig *InstanceGroup) taskOutcome(ctx context.Context, task *proxmox.Task) error {
	err := classifyTask(task.Status, task.ExitStatus)
	if err == nil {
		return nil
	}

	// A task that reported no exit status has no log worth fetching: the same blank or
	// unauthorized response that hid the status hides the log too. Otherwise fetch it best
	// effort -- a failure to fetch is reported alongside the task failure, never in place of it.
	if task.ExitStatus != "" {
		logLines, logErr := taskLog(ctx, task)

		ig.log.Error("Proxmox task failed", "upid", task.UPID, "exitstatus", task.ExitStatus, "log", logLines, "logerr", logErr)
	}

	return fmt.Errorf("task '%s': %w", task.UPID, err)
}

// taskLog returns the first page of a task's log in order. task.ExitStatus already carries
// Proxmox's own reason for a failure, so one page of context is enough for the log entry.
func taskLog(ctx context.Context, task *proxmox.Task) ([]string, error) {
	lines, err := task.Log(ctx, 0, taskLogLineLimit)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch log for task '%s': %w", task.UPID, err)
	}

	// proxmox.Log is keyed by absolute line number, so sorting the keys orders the page.
	ordered := make([]string, 0, len(lines))

	for _, number := range slices.Sorted(maps.Keys(lines)) {
		ordered = append(ordered, lines[number])
	}

	return ordered, nil
}
