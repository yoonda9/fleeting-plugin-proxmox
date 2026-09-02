package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/luthermonson/go-proxmox"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

var (
	ErrInstanceConnectionTimeout = errors.New("timed out getting connection info")
	ErrResumeFailed              = errors.New("one or more instances did not resume successfully")
	ErrSuspendFailed             = errors.New("one or more instances did not suspend successfully")
)

const (
	networkCheckTimeout = 5 * time.Second
	networkCheckRetries = 12
)

type InstanceGroup struct {
	Settings `json:",inline"`

	FleetingSettings provider.Settings `json:"-"`

	log     hclog.Logger    `json:"-"`
	proxmox *proxmox.Client `json:"-"`

	// This mutex is used when cloning template for new instances. It is required for blocking other
	// operations like collection or update, because when new instance is created with recycled ID then for
	// a brief period it will be reported from Proxmox with old name (e.g. InstanceNameRemoving).
	instanceCloningMu sync.Mutex `json:"-"`

	// Trigger for collector to start removed instances collection.
	instanceCollectionTrigger chan struct{} `json:"-"`

	// Trigger to shutdown collector.
	collectorShutdownTrigger chan struct{} `json:"-"`

	// Wait group for the collector.
	collectorWaitGroup sync.WaitGroup `json:"-"`

	// Trigger to shutdown session ticket refresher.
	sessionTicketRefresherShutdownTrigger chan struct{} `json:"-"`

	// Wait group for session ticket refresher.
	sessionTicketRefresherWaitGroup sync.WaitGroup `json:"-"`

	// Guards the shutdown triggers so Shutdown can be called more than once.
	shutdownOnce sync.Once `json:"-"`
}

// Init implements provider.InstanceGroup.
func (ig *InstanceGroup) Init(ctx context.Context, logger hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	ig.log = logger
	ig.FleetingSettings = settings
	ig.instanceCollectionTrigger = make(chan struct{}, 1)
	ig.collectorShutdownTrigger = make(chan struct{}, 1)
	ig.sessionTicketRefresherShutdownTrigger = make(chan struct{}, 1)

	err := ig.CheckRequiredFields()
	if err != nil {
		return provider.ProviderInfo{}, err
	}

	ig.FillWithDefaults()

	if ig.InsecureSkipTLSVerify {
		ig.log.Warn("TLS verification for Proxmox client is disabled, connections will be insecure")
	}

	ig.proxmox, err = ig.getProxmoxClient()
	if err != nil {
		return provider.ProviderInfo{}, err
	}

	err = ig.markStaleInstancesForRemoval(ctx)
	if err != nil {
		return provider.ProviderInfo{}, err
	}

	// Sleep for a bit to give Proxmox a chance to propagate renames for stale instances
	// Without this sleep these instances would be reported as creating during first Update
	<-time.After(collectionWaitAfterTrigger)

	//nolint:contextcheck
	ig.startRemovedInstanceCollector()

	//nolint:contextcheck
	ig.startSessionTicketRefresher()

	return provider.ProviderInfo{
		ID:      ig.Pool,
		MaxSize: *ig.MaxInstances,
	}, nil
}

// Shutdown implements provider.InstanceGroup.
//
// The InstanceGroup is single-use: the Once outlives any later Init, so a second
// Init/Shutdown cycle on the same value would leak its workers. The plugin host runs one
// Init and one Shutdown per process, which is the lifecycle this relies on.
func (ig *InstanceGroup) Shutdown(_ context.Context) error {
	ig.shutdownOnce.Do(func() {
		// The plugin host issues Shutdown even when Init never ran, and closing a nil channel
		// panics, which would take the whole plugin process down. Both channels are only ever
		// created together in Init, so one check covers them.
		if ig.collectorShutdownTrigger == nil {
			return
		}

		close(ig.collectorShutdownTrigger)
		close(ig.sessionTicketRefresherShutdownTrigger)
	})

	ig.collectorWaitGroup.Wait()
	ig.sessionTicketRefresherWaitGroup.Wait()

	return nil
}

// runParallel calls run once for every index in [0, count), returning how many calls failed
// alongside each call's error in its own slot, so a failure stays matched to the item that
// caused it without a lock.
func runParallel(count int, run func(index int) error) (int, []error) {
	if count <= 0 {
		return 0, nil
	}

	var waitGroup sync.WaitGroup

	errs := make([]error, count)

	for index := range count {
		waitGroup.Go(func() {
			errs[index] = run(index)
		})
	}

	waitGroup.Wait()

	failed := 0

	for _, err := range errs {
		if err != nil {
			failed++
		}
	}

	return failed, errs
}

// Increase implements provider.InstanceGroup.
func (ig *InstanceGroup) Increase(ctx context.Context, count int) (int, error) {
	template, err := ig.getProxmoxVM(ctx, *ig.TemplateID)
	if err != nil {
		return 0, fmt.Errorf("failed to find template with id='%d': %w", *ig.TemplateID, err)
	}

	// We need to mutex cloning as Proxmox will fail multiple requests in parallel
	cloneMu := new(sync.Mutex)

	ig.instanceCloningMu.Lock()
	defer ig.instanceCloningMu.Unlock()

	failed, errs := runParallel(count, func(_ int) error {
		vmid, err := ig.deployInstance(ctx, template, cloneMu)
		if err != nil {
			ig.log.Error("failed to deploy an instance", "vmid", vmid, "err", err)

			return err
		}

		ig.log.Info("successfully deployed instance", "vmid", vmid)

		return nil
	})

	succeeded := count - failed

	return succeeded, ig.batchError("failed to deploy some instances", succeeded, failed, errs)
}

// Update implements provider.InstanceGroup.
func (ig *InstanceGroup) Update(ctx context.Context, update func(instance string, state provider.State)) error {
	ig.instanceCloningMu.Lock()
	defer ig.instanceCloningMu.Unlock()

	pool, err := ig.getProxmoxPool(ctx)
	if err != nil {
		return err
	}

	for _, member := range pool.Members {
		if !ig.isProxmoxResourceAnInstance(member) {
			continue
		}

		var state provider.State

		switch member.Name {
		case ig.InstanceNameCreating:
			state = provider.StateCreating
		case ig.InstanceNameRunning:
			state = provider.StateRunning
		case ig.InstanceNameRemoving:
			state = provider.StateDeleting
		default:
			continue // Unknown name, skipping...
		}

		update(strconv.FormatUint(member.VMID, 10), state)
	}

	return nil
}

// ConnectInfo implements provider.InstanceGroup.
func (ig *InstanceGroup) ConnectInfo(ctx context.Context, instance string) (provider.ConnectInfo, error) {
	VMID, err := strconv.Atoi(instance)
	if err != nil {
		return provider.ConnectInfo{}, fmt.Errorf("failed to parse instance name '%s': %w", instance, err)
	}

	vm, err := ig.getProxmoxVM(ctx, VMID)
	if err != nil {
		return provider.ConnectInfo{}, fmt.Errorf("failed to retrieve instance vmid='%d': %w", VMID, err)
	}

	return ig.getConnectInfoFromVM(ctx, instance, vm)
}

// Decrease implements provider.InstanceGroup.
func (ig *InstanceGroup) Decrease(ctx context.Context, instancesToRemove []string) ([]string, error) {
	pool, err := ig.getProxmoxPool(ctx)
	if err != nil {
		return []string{}, err
	}

	// Only members named in instancesToRemove can reach either slice, so that is the bound to
	// size them by -- the pool holds the whole fleet and is typically far larger.
	var (
		succeeded = make([]string, 0, len(instancesToRemove))
		toRemove  = make([]*proxmox.ClusterResource, 0, len(instancesToRemove))
	)

	for _, member := range pool.Members {
		if !ig.isProxmoxResourceAnInstance(member) {
			continue
		}

		vmid := strconv.FormatUint(member.VMID, 10)

		if !slices.Contains(instancesToRemove, vmid) {
			continue
		}

		if member.Name == ig.InstanceNameCreating {
			// It must be running to start the deletion
			continue
		}

		if member.Name == ig.InstanceNameRemoving {
			// Already deleting...
			succeeded = append(succeeded, vmid)

			continue
		}

		ig.log.Info("removing instance", "vmid", member.VMID)

		toRemove = append(toRemove, &member)
	}

	failed, errs := runParallel(len(toRemove), func(index int) error {
		return ig.markInstancesForRemoval(ctx, toRemove[index])
	})

	for index, member := range toRemove {
		if errs[index] == nil {
			succeeded = append(succeeded, strconv.FormatUint(member.VMID, 10))
		}
	}

	return succeeded, ig.batchError("failed to mark some instances for removal", len(succeeded), failed, errs)
}

func (ig *InstanceGroup) Heartbeat(ctx context.Context, instance string) error {
	vmid, err := strconv.Atoi(instance)
	if err != nil {
		return fmt.Errorf("invalid vm id '%s': %w", instance, err)
	}

	vm, err := ig.getProxmoxVM(ctx, vmid)
	if err != nil {
		return err
	}

	// Returns an error if the QEMU agent is not communicating due to an empty result
	_, err = vm.AgentOsInfo(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to qemu agent '%s': %w", instance, err)
	}

	return nil
}

func (ig *InstanceGroup) Resume(ctx context.Context, instances []string) ([]string, error) {
	var succeeded []string

	var errs []string

	for _, instance := range instances {
		vmid, err := strconv.Atoi(instance)
		if err != nil {
			errs = append(errs, fmt.Sprintf("invalid vm id '%s'", instance))
			continue
		}

		vm, err := ig.getProxmoxVM(ctx, vmid)
		if err != nil {
			errs = append(errs, fmt.Sprintf("no vm with id '%d'", vmid))
			continue
		}

		_, err = vm.Resume(ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("resume api call failed for vm id '%d'", vmid))
			continue
		}

		succeeded = append(succeeded, instance)
	}

	if len(errs) > 0 {
		return succeeded, fmt.Errorf("%w: %s", ErrResumeFailed, strings.Join(errs, ", "))
	}

	return succeeded, nil
}

func (ig *InstanceGroup) Suspend(ctx context.Context, instances []string) ([]string, error) {
	var succeeded []string

	var errs []string

	for _, instance := range instances {
		vmid, err := strconv.Atoi(instance)
		if err != nil {
			errs = append(errs, fmt.Sprintf("invalid vm id '%s'", instance))
			continue
		}

		vm, err := ig.getProxmoxVM(ctx, vmid)
		if err != nil {
			errs = append(errs, fmt.Sprintf("no vm with id '%d'", vmid))
			continue
		}

		_, err = vm.Pause(ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("pause api call failed for vm id '%d'", vmid))
			continue
		}

		succeeded = append(succeeded, instance)
	}

	if len(errs) > 0 {
		return succeeded, fmt.Errorf("%w: %s", ErrSuspendFailed, strings.Join(errs, ", "))
	}

	return succeeded, nil
}

// batchError reports a batch of instance operations as failed only when nothing at all
// succeeded, and logs the aggregate trace that decision hides. It is the one place that rule
// lives, for both Increase and Decrease.
//
// A partial success is reported with a NIL error. fleeting's gRPC shim returns the response
// alongside the error and grpc-go discards the response whenever the error is non-nil, so a
// partial success reported as an error reaches the provisioner as no instances at all --
// leaving instances that were in fact created or removed untracked and classified
// CauseUnexpected. Each individual failure is already logged where it happened, so the warning
// here only has to record how much the nil error is hiding.
//
// succeeded is a parameter rather than counted from errs because Decrease also counts instances
// that were already being removed, which errs knows nothing about. An empty errs means nothing
// was attempted, which errors.Join already reports as success.
func (ig *InstanceGroup) batchError(msg string, succeeded, failed int, errs []error) error {
	if succeeded == 0 {
		return errors.Join(errs...)
	}

	if failed > 0 {
		ig.log.Warn(msg, "attempted", len(errs), "failed", failed)
	}

	return nil
}

func (ig *InstanceGroup) getConnectInfoFromVM(ctx context.Context, instance string, vm *proxmox.VirtualMachine) (provider.ConnectInfo, error) {
	for retry := range networkCheckRetries {
		networkInterfaces, err := vm.AgentGetNetworkIFaces(ctx)
		if err != nil {
			return provider.ConnectInfo{}, fmt.Errorf("failed to retrieve instance vmid='%d' interfaces: %w", vm.VMID, err)
		}

		internalAddress, externalAddress, err := determineAddresses(networkInterfaces, ig.InstanceNetworkInterface, ig.InstanceNetworkProtocol)
		if err != nil {
			ig.log.Error("failed to get network interface", "retry", retry, "vmid", vm.VMID, "err", err)
			time.Sleep(networkCheckTimeout)

			continue
		}

		return provider.ConnectInfo{
			ID:              instance,
			InternalAddr:    internalAddress,
			ExternalAddr:    externalAddress,
			ConnectorConfig: ig.FleetingSettings.ConnectorConfig,
		}, nil
	}

	return provider.ConnectInfo{}, fmt.Errorf("%w vmid='%d'", ErrInstanceConnectionTimeout, vm.VMID)
}
