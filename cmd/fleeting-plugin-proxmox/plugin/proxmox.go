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
	"time"

	"github.com/luthermonson/go-proxmox"
)

var ErrNotFound = errors.New("not found")

const proxmoxUserAgent = "fleeting-plugin-proxmox"

// proxmoxRetryBackoff is the base delay between retries of a read-only Proxmox API
// call inside retryIdempotent; it doubles after each attempt. It is not a setting:
// an operator reasons about how many attempts to allow (proxmox_api_retry_attempts),
// not the spacing between them.
const proxmoxRetryBackoff = 500 * time.Millisecond

// proxmoxRetryMaxBackoff caps the exponential backoff between retries, so a large attempt
// count can never produce an overlong sleep.
const proxmoxRetryMaxBackoff = 30 * time.Second

// backoffForAttempt returns the delay before the retry that follows attempt (0-indexed):
// base, doubling per attempt, capped at proxmoxRetryMaxBackoff.
func backoffForAttempt(base time.Duration, attempt int) time.Duration {
	wait := base

	for range attempt {
		if wait >= proxmoxRetryMaxBackoff {
			break
		}

		wait *= 2
	}

	return min(wait, proxmoxRetryMaxBackoff)
}

// retryIdempotent calls attemptFn up to attempts times in total - the first call
// included, so attempts=1 never retries - for as long as the failure classifies as
// transient, with exponential backoff between attempts. It
// stops immediately, without retrying, once ctx is done. A non-positive attempts is
// treated as 1, so fn is always called at least once rather than no-oping to nil.
//
// Only ever wrap this around read-only calls. go-proxmox's own retry transport
// (proxmox.WithRetry) is deliberately not used anywhere in this plugin because it
// buffers and retries POST bodies too - a transient failure on a write such as
// clone could then be replayed and create a second VM. retryIdempotent has no such
// problem only because every call site wrapping it is a GET.
func retryIdempotent(ctx context.Context, attempts int, backoff time.Duration, attemptFn func() error) error {
	for attempt := 0; ; attempt++ {
		err := attemptFn()
		if err == nil {
			return nil
		}

		if ctx.Err() != nil || classifyError(err) != classTransient || attempt >= attempts-1 {
			return err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("retry aborted: %w", ctx.Err())
		case <-time.After(backoffForAttempt(backoff, attempt)):
		}
	}
}

func (ig *InstanceGroup) getProxmoxPool(ctx context.Context) (*proxmox.Pool, error) {
	pool, err := ig.proxmox.Pool(ctx, ig.Pool)
	if err != nil {
		return nil, fmt.Errorf("failed to get pool id='%s': %w", ig.Pool, err)
	}

	return pool, nil
}

// Where possible, use getProxmoxVMOnNode instead as it makes less calls to API.
func (ig *InstanceGroup) getProxmoxVM(ctx context.Context, vmid int) (*proxmox.VirtualMachine, error) {
	pool, err := ig.getProxmoxPool(ctx)
	if err != nil {
		return nil, err
	}

	for _, member := range pool.Members {
		if member.Type != vmTypeQEMU {
			continue
		}

		if member.VMID == uint64(vmid) {
			return ig.getProxmoxVMOnNode(ctx, vmid, member.Node)
		}
	}

	return nil, ErrNotFound
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

func (ig *InstanceGroup) getProxmoxClient() (*proxmox.Client, error) {
	url, err := url.Parse(ig.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL='%s': %w", ig.URL, err)
	}

	credentials, err := ig.getProxmoxCredentials()
	if err != nil {
		return nil, err
	}

	transport := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		//nolint:gosec
		InsecureSkipVerify: ig.InsecureSkipTLSVerify,
	}
	transport.MaxIdleConnsPerHost = *ig.HTTPMaxIdleConnsPerHost

	return proxmox.NewClient(
		url.JoinPath("/api2/json").String(),
		proxmox.WithCredentials(credentials),
		proxmox.WithHTTPClient(&http.Client{Transport: transport}),
		proxmox.WithTimeout(seconds(ig.HTTPTimeout)),
		proxmox.WithUserAgent(proxmoxUserAgent),
		proxmox.WithEagerAuth(),
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
