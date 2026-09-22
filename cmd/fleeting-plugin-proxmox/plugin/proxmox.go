package plugin

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/luthermonson/go-proxmox"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

var (
	ErrNotFound = errors.New("not found")
	// ErrNotOwned reports a VM that is in the pool but does not carry one of this
	// config's instance names.
	ErrNotOwned = errors.New("not owned by this instance group")
)

func notOwned(vmid uint64, name string) error {
	return fmt.Errorf("vmid='%d' is named %q: %w", vmid, name, ErrNotOwned)
}

func (ig *InstanceGroup) getProxmoxPool(ctx context.Context) (*proxmox.Pool, error) {
	pool, err := ig.proxmox.Pool(ctx, ig.Pool)
	if err != nil {
		return nil, fmt.Errorf("failed to get pool id='%s': %w", ig.Pool, err)
	}

	return pool, nil
}

func (ig *InstanceGroup) findPoolMember(ctx context.Context, vmid int) (proxmox.ClusterResource, error) {
	pool, err := ig.getProxmoxPool(ctx)
	if err != nil {
		return proxmox.ClusterResource{}, err
	}

	for _, member := range pool.Members {
		if member.Type != vmTypeQEMU {
			continue
		}

		if member.VMID == uint64(vmid) {
			return member, nil
		}
	}

	return proxmox.ClusterResource{}, ErrNotFound
}

// Where possible, use getProxmoxVMOnNode instead as it makes less calls to API. It does not
// check ownership; lifecycle operations on instances go through ownedInstance instead.
func (ig *InstanceGroup) getProxmoxVM(ctx context.Context, vmid int) (*proxmox.VirtualMachine, error) {
	member, err := ig.findPoolMember(ctx, vmid)
	if err != nil {
		return nil, err
	}

	return ig.getProxmoxVMOnNode(ctx, vmid, member.Node)
}

func (ig *InstanceGroup) getProxmoxVMOnNode(ctx context.Context, vmid int, nodeName string) (*proxmox.VirtualMachine, error) {
	node, err := ig.proxmox.Node(ctx, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node='%s': %w", nodeName, err)
	}

	vm, err := node.VirtualMachine(ctx, vmid)
	if err != nil {
		return nil, fmt.Errorf("failed to get vm='%d' on node='%s': %w", vmid, nodeName, err)
	}

	return vm, nil
}

// fetchedName returns the name the VM carries: the config name (what a rename writes) when
// present, else the status name.
func fetchedName(vm *proxmox.VirtualMachine) string {
	if vm.VirtualMachineConfig != nil && vm.VirtualMachineConfig.Name != "" {
		return vm.VirtualMachineConfig.Name
	}

	return vm.Name
}

// stateForName maps one of this group's instance names to its state; the bool is false for any
// other name.
func (ig *InstanceGroup) stateForName(name string) (provider.State, bool) {
	switch name {
	case ig.InstanceNameCreating:
		return provider.StateCreating, true
	case ig.InstanceNameRunning:
		return provider.StateRunning, true
	case ig.InstanceNameRemoving:
		return provider.StateDeleting, true
	default:
		return "", false
	}
}

func (ig *InstanceGroup) isOwnName(name string) bool {
	_, ok := ig.stateForName(name)

	return ok
}

func (ig *InstanceGroup) ownedInstance(ctx context.Context, vmid int) (*proxmox.VirtualMachine, error) {
	member, err := ig.findPoolMember(ctx, vmid)
	if err != nil {
		return nil, err
	}

	// An empty listed name is no evidence either way: Proxmox leaves it unset while it has no
	// fresh status for the VM. The fetched name below decides.
	if member.Name != "" && !ig.isOwnName(member.Name) {
		return nil, notOwned(member.VMID, member.Name)
	}

	vm, err := ig.getProxmoxVMOnNode(ctx, vmid, member.Node)
	if err != nil {
		return nil, err
	}

	if name := fetchedName(vm); !ig.isOwnName(name) {
		return nil, notOwned(member.VMID, name)
	}

	return vm, nil
}

func (ig *InstanceGroup) getProxmoxClient() (*proxmox.Client, error) {
	url, err := url.Parse(ig.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL='%s': %w", ig.URL, err)
	}

	credentials, err := ig.getProxmoxCredentials()
	if err != nil {
		return nil, err
	}

	httpClient := http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				//nolint:gosec
				InsecureSkipVerify: ig.InsecureSkipTLSVerify,
			},
		},
	}

	return proxmox.NewClient(
		url.JoinPath("/api2/json").String(),
		proxmox.WithCredentials(credentials),
		proxmox.WithHTTPClient(&httpClient),
	), nil
}

func (ig *InstanceGroup) getProxmoxCredentials() (*proxmox.Credentials, error) {
	credentialsFile, err := os.Open(ig.CredentialsFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open credentials file from path='%s': %w", ig.CredentialsFilePath, err)
	}
	defer credentialsFile.Close()

	credentials := proxmox.Credentials{}

	err = json.NewDecoder(credentialsFile).Decode(&credentials)
	if err != nil {
		return nil, fmt.Errorf("failed to decode credentials file from path='%s': %w", ig.CredentialsFilePath, err)
	}

	return &credentials, nil
}
