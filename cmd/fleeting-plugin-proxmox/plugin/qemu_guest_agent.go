package plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/luthermonson/go-proxmox"
)

// ErrAgentStartTimeout is returned when the QEMU guest agent never reports ready
// within instance_agent_start_timeout.
var ErrAgentStartTimeout = errors.New("timed out waiting for qemu guest agent to start")

// waitForAgent replaces go-proxmox's VirtualMachine.WaitForAgent, which gives up on
// any error except the literal "500 QEMU guest agent is not running" (vendor
// virtual_machine.go) - exactly the errors a saturated node produces: a
// guest-get-osinfo QMP timeout under load, a pveproxy 5xx during overload, or a
// transient TLS error. This loop keeps waiting through both classAgentNotReady and
// classTransient, and fails immediately on anything classTerminal.
func (ig *InstanceGroup) waitForAgent(ctx context.Context, vm *proxmox.VirtualMachine) error {
	ctx, cancel := context.WithTimeout(ctx, seconds(ig.InstanceAgentStartTimeout))
	defer cancel()

	ticker := time.NewTicker(proxmox.DefaultWaitInterval)
	defer ticker.Stop()

	// lastErr holds the most recent answer Proxmox itself gave, because on a timeout that is
	// the operator's only sight of the cause. A poll the budget cut short reports the
	// cancellation rather than an answer, so it must never overwrite this.
	var lastErr error

	for {
		_, err := vm.AgentOsInfo(ctx)

		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return agentStartTimeout(vm.VMID, lastErr, err)
		case classifyError(err) == classTerminal:
			return fmt.Errorf("failed waiting for qemu agent on vmid='%d': %w", vm.VMID, err)
		}

		lastErr = err

		select {
		case <-ctx.Done():
			return agentStartTimeout(vm.VMID, lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

// agentStartTimeout reports that the agent never came up inside
// instance_agent_start_timeout, naming the last answer Proxmox gave. Every reason Proxmox has
// for refusing the agent arrives as a 500 whose message is the only thing that tells them
// apart -- a template with no guest agent configured, or a VM that never started, look exactly
// like an agent still booting -- so the wait spends its whole budget either way and this
// message is what distinguishes them afterwards. ended is the fallback for a wait that was
// cancelled before Proxmox answered even once.
func agentStartTimeout(vmid proxmox.StringOrUint64, last, ended error) error {
	if last == nil {
		last = ended
	}

	return fmt.Errorf("%w: vmid='%d': last error: %w", ErrAgentStartTimeout, vmid, last)
}
