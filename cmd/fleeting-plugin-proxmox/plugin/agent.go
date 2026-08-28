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

// agentPollInterval is the cadence of waitForAgent's poll loop; it mirrors
// go-proxmox's own DefaultWaitInterval, which VirtualMachine.WaitForAgent used.
const agentPollInterval = 1 * time.Second

// waitForAgent replaces go-proxmox's VirtualMachine.WaitForAgent, which gives up on
// any error except the literal "500 QEMU guest agent is not running" (vendor
// virtual_machine.go) - exactly the errors a saturated node produces: a
// guest-get-osinfo QMP timeout under load, a pveproxy 5xx during overload, or a
// transient TLS error. This loop keeps waiting through both classAgentNotReady and
// classTransient, and fails immediately on anything classTerminal.
func (ig *InstanceGroup) waitForAgent(ctx context.Context, vm *proxmox.VirtualMachine) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(*ig.InstanceAgentStartTimeout)*time.Second)
	defer cancel()

	ticker := time.NewTicker(agentPollInterval)
	defer ticker.Stop()

	for {
		_, err := vm.AgentOsInfo(ctx)

		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return fmt.Errorf("%w: vmid='%d'", ErrAgentStartTimeout, vm.VMID)
		case classifyError(err) == classTerminal:
			return fmt.Errorf("failed waiting for qemu agent on vmid='%d': %w", vm.VMID, err)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: vmid='%d'", ErrAgentStartTimeout, vm.VMID)
		case <-ticker.C:
		}
	}
}
