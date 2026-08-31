package plugin

import (
	"context"
	"errors"
	"fmt"

	"github.com/luthermonson/go-proxmox"
	"golang.org/x/sync/errgroup"
)

const (
	vmOptName = "name"
	vmOptTags = "tags"

	// proxmoxResourceTypeQemu is the ClusterResource.Type / VirtualMachine.Type
	// value Proxmox uses for QEMU VMs (as opposed to "lxc" containers etc.).
	proxmoxResourceTypeQemu = "qemu"
)

var ErrCloneVMWithoutConfiguredStorage = errors.New("attempted to clone a VM without configured storage")

func (ig *InstanceGroup) deployInstance(ctx context.Context, template *proxmox.VirtualMachine) (int, error) {
	VMID, err := ig.cloneAndWaitForTemplate(ctx, template)
	if err != nil {
		return VMID, fmt.Errorf("failed to deploy instance: %w", err)
	}

	vm, err := ig.getProxmoxVM(ctx, VMID)
	if err != nil {
		return VMID, fmt.Errorf("failed to find newly deployed instance vmid='%d': %w", VMID, err)
	}

	_, err = vm.Config(ctx, proxmox.VirtualMachineOption{
		Name:  vmOptTags,
		Value: ig.InstanceTagsCreating,
	})
	if err != nil {
		return VMID, fmt.Errorf("failed to get instance config vmid='%d': %w", VMID, err)
	}

	// Start, configure etc.
	err = func() error {
		if ig.InstanceAutoresizeSize != "" {
			task, err := vm.ResizeDisk(ctx, ig.InstanceAutoresizeDisk, ig.InstanceAutoresizeSize)
			if err == nil {
				err = ig.waitTask(ctx, task, ig.taskWaitTimeout())
			}

			if err != nil {
				return fmt.Errorf("failed to resize disk: %w", err)
			}
		}

		// Start the VM
		task, err := vm.Start(ctx)
		if err == nil {
			err = ig.waitTask(ctx, task, ig.taskWaitTimeout())
		}

		if err != nil {
			return fmt.Errorf("failed to start newly deployed instance: %w", err)
		}

		// Wait for agent to start
		err = ig.waitForAgent(ctx, vm)
		if err != nil {
			return fmt.Errorf("failed when waiting for qemu agent to start on newly deployed instance: %w", err)
		}

		return nil
	}()

	newInstanceName := ig.InstanceNameRunning
	newInstanceTags := ig.InstanceTagsRunning

	if err != nil {
		ig.log.Error("instance deployment failed, marking for removal", "vmid", VMID, "err", err)
		newInstanceName = ig.InstanceNameRemoving
		newInstanceTags = ig.InstanceTagsRemoving
	}

	_, renameErr := vm.Config(ctx,
		proxmox.VirtualMachineOption{
			Name:  vmOptName,
			Value: newInstanceName,
		},
		proxmox.VirtualMachineOption{
			Name:  vmOptTags,
			Value: newInstanceTags,
		},
	)
	if renameErr != nil {
		ig.log.Error("failed to rename instance", "vmid", VMID, "err", renameErr)
	}

	if err != nil {
		return VMID, fmt.Errorf("failed to configure instance, marked for removal due to: %w", err)
	}

	return VMID, nil
}

// cloneConcurrencySemaphore lazily builds the buffered channel that bounds
// concurrent clone tasks, sized from clone_concurrency. Lazy so InstanceGroup
// values built directly (e.g. in tests, without Init/FillWithDefaults) still
// get a working, correctly-sized bound instead of blocking on a nil channel.
func (ig *InstanceGroup) cloneConcurrencySemaphore() chan struct{} {
	ig.cloneSemaphoreOnce.Do(func() {
		concurrency := DefaultCloneConcurrency
		if ig.CloneConcurrency != nil {
			concurrency = *ig.CloneConcurrency
		}

		ig.cloneSemaphore = make(chan struct{}, concurrency)
	})

	return ig.cloneSemaphore
}

// cloneAndWaitForTemplate clones the template and waits for the clone task to
// complete, bounded by clone_concurrency. The semaphore is held across
// waitTask, not just the POST: Clone returns a UPID immediately and the disk
// copy happens in Proxmox's forked worker, so a semaphore around the POST
// alone would bound nothing.
func (ig *InstanceGroup) cloneAndWaitForTemplate(ctx context.Context, template *proxmox.VirtualMachine) (int, error) {
	semaphore := ig.cloneConcurrencySemaphore()

	select {
	case semaphore <- struct{}{}:
	case <-ctx.Done():
		return -1, fmt.Errorf("failed to acquire a clone concurrency slot: %w", ctx.Err())
	}

	defer func() { <-semaphore }()

	VMID, task, err := ig.cloneTemplate(ctx, template)
	if err != nil {
		return VMID, err
	}

	ig.log.Info("Deploying new instance", "vmid", VMID)

	err = ig.waitTask(ctx, task, ig.taskWaitTimeout())
	if err != nil {
		// The clone POST succeeded, so VMID is allocated and Proxmox rolls the
		// target config back when the clone worker fails - but a wait that
		// merely timed out may still land, so ReleaseIfFree reconfirms with the
		// cluster before giving the reservation back rather than risk handing a
		// live id to another clone.
		ig.vmids.ReleaseIfFree(ctx, VMID)
	}

	return VMID, err
}

func (ig *InstanceGroup) cloneTemplate(ctx context.Context, template *proxmox.VirtualMachine) (int, *proxmox.Task, error) {
	cloneOptions, err := ig.getTemplateCloneOptions(template)
	if err != nil {
		return -1, nil, err
	}

	vmid, err := ig.vmids.Allocate(ctx)
	if err != nil {
		return -1, nil, fmt.Errorf("failed to allocate a vmid for the clone: %w", err)
	}

	cloneOptions.NewID = vmid

	VMID, task, err := template.Clone(ctx, cloneOptions)
	if err != nil {
		ig.vmids.Release(vmid)

		return -1, nil, fmt.Errorf("failed to clone the template: %w", err)
	}

	return VMID, task, nil
}

func (ig *InstanceGroup) getTemplateCloneOptions(template *proxmox.VirtualMachine) (*proxmox.VirtualMachineCloneOptions, error) {
	cloneOptions := &proxmox.VirtualMachineCloneOptions{
		Name:    ig.InstanceNameCreating,
		Pool:    ig.Pool,
		Storage: ig.Storage,
		Full:    true,
	}

	if ig.CloneBandwidthLimit != nil && *ig.CloneBandwidthLimit > 0 {
		bwLimit := uint64(*ig.CloneBandwidthLimit)
		cloneOptions.BWLimit = &bwLimit
	}

	if !template.Template && ig.Storage == "" {
		return nil, ErrCloneVMWithoutConfiguredStorage
	}

	if template.Template && ig.Storage == "" {
		cloneOptions.Full = false
	}

	return cloneOptions, nil
}

func (ig *InstanceGroup) markStaleInstancesForRemoval(ctx context.Context) error {
	pool, err := ig.getProxmoxPool(ctx)
	if err != nil {
		return err
	}

	instancesToMarkForRemoval := make([]*proxmox.ClusterResource, 0, len(pool.Members))

	for _, member := range pool.Members {
		if !ig.isProxmoxResourceAnInstance(member) {
			continue
		}

		if member.Name != ig.InstanceNameCreating {
			continue
		}

		ig.log.Info("Found stale instance, marking for removal", "name", member.Name, "vmid", member.VMID, "node", member.Node)
		instancesToMarkForRemoval = append(instancesToMarkForRemoval, &member)
	}

	if len(instancesToMarkForRemoval) < 1 {
		return nil
	}

	err = ig.markInstancesForRemoval(ctx, instancesToMarkForRemoval...)
	if err != nil {
		return fmt.Errorf("failed to mark stale instances for removal: %w", err)
	}

	return nil
}

func (ig *InstanceGroup) markInstancesForRemoval(ctx context.Context, instances ...*proxmox.ClusterResource) error {
	var errorGroup errgroup.Group

	for _, instance := range instances {
		errorGroup.Go(func() error {
			log := ig.log.With("name", instance.Name, "vmid", instance.VMID, "node", instance.Node)

			vm, err := ig.getProxmoxVMOnNode(ctx, int(instance.VMID), instance.Node)
			if err != nil {
				log.Error("Failed to mark instance for removal", "err", err)
				return fmt.Errorf("failed to mark instance for removal: %w", err)
			}

			task, err := vm.Config(ctx,
				proxmox.VirtualMachineOption{
					Name:  vmOptName,
					Value: ig.InstanceNameRemoving,
				},
				proxmox.VirtualMachineOption{
					Name:  vmOptTags,
					Value: ig.InstanceTagsRemoving,
				},
			)
			if err == nil {
				err = ig.waitTask(ctx, task, ig.taskWaitTimeout())
			}

			if err != nil {
				log.Error("Failed to mark instance for removal", "err", err)
				return fmt.Errorf("failed to mark instance for removal: %w", err)
			}

			return nil
		})
	}

	err := errorGroup.Wait()
	if err != nil {
		ig.triggerCollection()
		return fmt.Errorf("failed to mark one or more instances for removal: %w", err)
	}

	ig.triggerCollection()

	return nil
}

func (ig *InstanceGroup) isProxmoxResourceAnInstance(member proxmox.ClusterResource) bool {
	return member.Type == proxmoxResourceTypeQemu && member.VMID != uint64(*ig.TemplateID)
}
