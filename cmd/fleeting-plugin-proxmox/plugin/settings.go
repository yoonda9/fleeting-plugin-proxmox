package plugin

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
)

var (
	ErrRequiredSettingMissing  = errors.New("required setting is missing")
	ErrSettingInvalidParameter = errors.New("setting has invalid parameter")
)

// NetworkProtocol: Available network protocols to look for when discovering instance's IP address.
type NetworkProtocol = string

const (
	// NetworkProtocolIPv4: Tries to find one internal and one external IPv4 address.
	NetworkProtocolIPv4 NetworkProtocol = "ipv4"

	// NetworkProtocolIPv6: Tries to find one internal (ULA) and one global (GUA) IPv6 address.
	NetworkProtocolIPv6 NetworkProtocol = "ipv6"

	// NetworkProtocolAny: Will prioritize IPv6 but return IPv4 if there is no IPv6.
	NetworkProtocolAny NetworkProtocol = "any"
)

// Default values for plugin settings.
const (
	DefaultInstanceNetworkProtocol = NetworkProtocolIPv4

	DefaultInstanceNameCreating = "fleeting-creating"
	DefaultInstanceNameRunning  = "fleeting-running"
	DefaultInstanceNameRemoving = "fleeting-removing"

	DefaultProxmoxTaskWaitInterval   int = 10
	DefaultProxmoxTaskWaitTimeout    int = 300
	DefaultInstanceAgentStartTimeout int = 120
	DefaultInstanceConnectTimeout    int = 60
	DefaultCollectorInterval         int = 60
	DefaultHTTPTimeout               int = 60
	DefaultHTTPMaxIdleConnsPerHost   int = 8
	DefaultProxmoxAPIRetryAttempts   int = 3
)

// Disk index limits for each disk type.
const (
	maxIDEIndex    = 3
	maxSCSIIndex   = 30
	maxSATAIndex   = 5
	maxVIRTIOIndex = 15
)

// Settings: Plguin settings.
type Settings struct {
	// Proxmox VE URL.
	URL string `json:"url"`

	// If true then TLS certificate verification is disabled.
	InsecureSkipTLSVerify bool `json:"insecure_skip_tls_verify"`

	// Path to Proxmox VE credentials file.
	CredentialsFilePath string `json:"credentials_file_path"`

	// Name of the Proxmox VE pool to use.
	Pool string `json:"pool"`

	// Name of the Proxmox VE storage to use.
	Storage string `json:"storage"`

	// ID of the Proxmox VE VM to create instances from.
	TemplateID *int `json:"template_id,omitempty"`

	// Maximum instances than can be deployed.
	MaxInstances *int `json:"max_instances,omitempty"`

	// Network interface to read instance's IP address from.
	InstanceNetworkInterface string `json:"instance_network_interface"`

	// Network protocol to look for when discovering instance's IP address.
	InstanceNetworkProtocol NetworkProtocol `json:"instance_network_protocol"`

	// Name to set for instances during creation.
	InstanceNameCreating string `json:"instance_name_creating"`

	// Name to set for running instances.
	InstanceNameRunning string `json:"instance_name_running"`

	// Name to set for instances during removal.
	InstanceNameRemoving string `json:"instance_name_removing"`

	// Tags to set for instances during creation, semicolon delimited.
	InstanceTagsCreating string `json:"instance_tags_creating"`

	// Tags to set for running instances, semicolon delimited.
	InstanceTagsRunning string `json:"instance_tags_running"`

	// Tags to set for instances during removal, semicolon delimited.
	InstanceTagsRemoving string `json:"instance_tags_removing"`

	// Disk to increase after cloning.
	InstanceAutoresizeDisk string `json:"instance_autoresize_disk"`

	// Increase disk to this size after cloning.
	InstanceAutoresizeSize string `json:"instance_autoresize_size"`

	// How often should task status be queried
	ProxmoxTaskWaitInterval *int `json:"proxmox_task_wait_interval"`

	// How long to wait for a Proxmox task (clone, resize, start, stop, delete) to complete.
	ProxmoxTaskWaitTimeout *int `json:"proxmox_task_wait_timeout"`

	// How long to wait for the QEMU guest agent to start on a newly deployed instance.
	InstanceAgentStartTimeout *int `json:"instance_agent_start_timeout"`

	// How long to wait for a newly deployed instance to report a usable network address.
	InstanceConnectTimeout *int `json:"instance_connect_timeout"`

	// How often the collector polls for instances to remove.
	CollectorInterval *int `json:"collector_interval"`

	// Per-request deadline for calls to the Proxmox VE API.
	HTTPTimeout *int `json:"http_timeout"`

	// Maximum idle HTTP connections to keep open per Proxmox VE host.
	HTTPMaxIdleConnsPerHost *int `json:"http_max_idle_conns_per_host"`

	// How many times to retry a read-only Proxmox API call that failed transiently.
	ProxmoxAPIRetryAttempts *int `json:"proxmox_api_retry_attempts"`

	// Start (inclusive) of a dedicated VMID range for this manager. Unset uses
	// /cluster/nextid instead. Must be set together with VMIDRangeEnd.
	VMIDRangeStart *int `json:"vmid_range_start,omitempty"`

	// End (exclusive) of a dedicated VMID range for this manager. Unset uses
	// /cluster/nextid instead. Must be set together with VMIDRangeStart.
	VMIDRangeEnd *int `json:"vmid_range_end,omitempty"`
}

func (s *Settings) FillWithDefaults() {
	if s.InstanceNetworkProtocol == "" {
		s.InstanceNetworkProtocol = DefaultInstanceNetworkProtocol
	}

	if s.InstanceNameCreating == "" {
		s.InstanceNameCreating = DefaultInstanceNameCreating
	}

	if s.InstanceNameRunning == "" {
		s.InstanceNameRunning = DefaultInstanceNameRunning
	}

	if s.InstanceNameRemoving == "" {
		s.InstanceNameRemoving = DefaultInstanceNameRemoving
	}

	if s.InstanceNetworkProtocol == "" {
		s.InstanceNetworkProtocol = DefaultInstanceNetworkProtocol
	}

	if s.ProxmoxTaskWaitInterval == nil {
		s.ProxmoxTaskWaitInterval = new(int)
		*s.ProxmoxTaskWaitInterval = DefaultProxmoxTaskWaitInterval
	}

	if s.ProxmoxTaskWaitTimeout == nil {
		s.ProxmoxTaskWaitTimeout = new(int)
		*s.ProxmoxTaskWaitTimeout = DefaultProxmoxTaskWaitTimeout
	}

	if s.InstanceAgentStartTimeout == nil {
		s.InstanceAgentStartTimeout = new(int)
		*s.InstanceAgentStartTimeout = DefaultInstanceAgentStartTimeout
	}

	if s.InstanceConnectTimeout == nil {
		s.InstanceConnectTimeout = new(int)
		*s.InstanceConnectTimeout = DefaultInstanceConnectTimeout
	}

	if s.CollectorInterval == nil {
		s.CollectorInterval = new(int)
		*s.CollectorInterval = DefaultCollectorInterval
	}

	if s.HTTPTimeout == nil {
		s.HTTPTimeout = new(int)
		*s.HTTPTimeout = DefaultHTTPTimeout
	}

	if s.HTTPMaxIdleConnsPerHost == nil {
		s.HTTPMaxIdleConnsPerHost = new(int)
		*s.HTTPMaxIdleConnsPerHost = DefaultHTTPMaxIdleConnsPerHost
	}

	if s.ProxmoxAPIRetryAttempts == nil {
		s.ProxmoxAPIRetryAttempts = new(int)
		*s.ProxmoxAPIRetryAttempts = DefaultProxmoxAPIRetryAttempts
	}
}

func (s *Settings) CheckRequiredFields() error {
	// Collect all validators
	validators := []struct {
		name     string
		validate func() error
	}{
		{"url", s.validateURL},
		{"credentials_file_path", s.validateCredentialsFilePath},
		{"pool", s.validatePool},
		{"template_id", s.validateTemplateID},
		{"vmid_range", s.validateVMIDRange},
		{"max_instances", s.validateMaxInstances},
		{"instance_network_protocol", s.validateInstanceNetworkProtocol},
		{"instance_autoresize_disk", s.validateInstanceAutoresizeDisk},
		{"instance_autoresize_size", s.validateInstanceAutoresizeSize},
		{"instance_autoresize_consistency", s.validateInstanceAutoresizeConsistency},
		{"proxmox_task_wait_timeout", s.validateProxmoxTaskWaitTimeout},
		{"instance_agent_start_timeout", s.validateInstanceAgentStartTimeout},
		{"instance_connect_timeout", s.validateInstanceConnectTimeout},
		{"collector_interval", s.validateCollectorInterval},
		{"http_timeout", s.validateHTTPTimeout},
		{"http_max_idle_conns_per_host", s.validateHTTPMaxIdleConnsPerHost},
		{"proxmox_api_retry_attempts", s.validateProxmoxAPIRetryAttempts},
	}

	for _, v := range validators {
		err := v.validate()
		if err != nil {
			return err
		}
	}

	return nil
}

// validateURL checks that the URL setting is not empty.
func (s *Settings) validateURL() error {
	if s.URL == "" {
		return fmt.Errorf("%w: url", ErrRequiredSettingMissing)
	}

	return nil
}

// validateCredentialsFilePath checks that the credentials file path is not empty.
func (s *Settings) validateCredentialsFilePath() error {
	if s.CredentialsFilePath == "" {
		return fmt.Errorf("%w: credentials_file_path", ErrRequiredSettingMissing)
	}

	return nil
}

// validatePool checks that the pool setting is not empty.
func (s *Settings) validatePool() error {
	if s.Pool == "" {
		return fmt.Errorf("%w: pool", ErrRequiredSettingMissing)
	}

	return nil
}

// validateTemplateID checks that the template ID is set.
func (s *Settings) validateTemplateID() error {
	if s.TemplateID == nil {
		return fmt.Errorf("%w: template_id", ErrRequiredSettingMissing)
	}

	return nil
}

// validateVMIDRange checks that vmid_range_start and vmid_range_end, if either is
// set, are both set, form a non-empty range (start < end), and exclude
// template_id. CheckRequiredFields runs validateTemplateID immediately before
// this validator, so TemplateID is guaranteed non-nil here.
func (s *Settings) validateVMIDRange() error {
	if s.VMIDRangeStart == nil && s.VMIDRangeEnd == nil {
		return nil
	}

	if s.VMIDRangeStart == nil || s.VMIDRangeEnd == nil {
		return fmt.Errorf("%w: vmid_range_start and vmid_range_end must both be set, or both left unset",
			ErrSettingInvalidParameter)
	}

	if *s.VMIDRangeStart >= *s.VMIDRangeEnd {
		return fmt.Errorf("%w: vmid_range_start must be less than vmid_range_end", ErrSettingInvalidParameter)
	}

	if *s.TemplateID >= *s.VMIDRangeStart && *s.TemplateID < *s.VMIDRangeEnd {
		return fmt.Errorf("%w: vmid_range_start..vmid_range_end must not contain template_id",
			ErrSettingInvalidParameter)
	}

	return nil
}

// validateMaxInstances checks that the max instances is set.
func (s *Settings) validateMaxInstances() error {
	if s.MaxInstances == nil {
		return fmt.Errorf("%w: max_instances", ErrRequiredSettingMissing)
	}

	return nil
}

// validateInstanceNetworkProtocol checks that the network protocol is valid.
func (s *Settings) validateInstanceNetworkProtocol() error {
	if s.InstanceNetworkProtocol == "" {
		return nil
	}

	validProtocols := []NetworkProtocol{NetworkProtocolIPv4, NetworkProtocolIPv6, NetworkProtocolAny}
	if slices.Contains(validProtocols, s.InstanceNetworkProtocol) {
		return nil
	}

	return fmt.Errorf("%w: instance_network_protocol: must be ipv4, ipv6 or any", ErrSettingInvalidParameter)
}

// validateInstanceAutoresizeDisk checks that the autoresize disk setting is valid.
func (s *Settings) validateInstanceAutoresizeDisk() error {
	if s.InstanceAutoresizeDisk == "" {
		return nil
	}

	diskRE := regexp.MustCompile(`^(ide|scsi|sata|virtio)(\d+)$`)

	matches := diskRE.FindStringSubmatch(s.InstanceAutoresizeDisk)
	if matches == nil {
		return fmt.Errorf("%w: instance_autoresize_disk: disk is not valid", ErrSettingInvalidParameter)
	}

	maxIndex := getDiskMaxIndex(matches[1])

	i, convErr := strconv.Atoi(matches[2])
	if convErr != nil {
		return fmt.Errorf("invalid integer for i=%q: %w", matches[2], convErr)
	}

	if i > maxIndex {
		return fmt.Errorf("%w: instance_autoresize_disk: disk type is valid, but index %s is not possible", ErrSettingInvalidParameter, matches[2])
	}

	return nil
}

// validatePositiveSeconds checks that an optional seconds-denominated setting, if set, is positive.
func validatePositiveSeconds(name string, value *int) error {
	if value != nil && *value <= 0 {
		return fmt.Errorf("%w: %s: must be a positive number of seconds", ErrSettingInvalidParameter, name)
	}

	return nil
}

// validateProxmoxTaskWaitTimeout checks that the task wait timeout, if set, is positive.
func (s *Settings) validateProxmoxTaskWaitTimeout() error {
	return validatePositiveSeconds("proxmox_task_wait_timeout", s.ProxmoxTaskWaitTimeout)
}

// validateInstanceAgentStartTimeout checks that the agent start timeout, if set, is positive.
func (s *Settings) validateInstanceAgentStartTimeout() error {
	return validatePositiveSeconds("instance_agent_start_timeout", s.InstanceAgentStartTimeout)
}

// validateInstanceConnectTimeout checks that the connect timeout, if set, is positive.
func (s *Settings) validateInstanceConnectTimeout() error {
	return validatePositiveSeconds("instance_connect_timeout", s.InstanceConnectTimeout)
}

// validateCollectorInterval checks that the collector interval, if set, is positive.
func (s *Settings) validateCollectorInterval() error {
	return validatePositiveSeconds("collector_interval", s.CollectorInterval)
}

// validateHTTPTimeout checks that the HTTP timeout, if set, is positive.
func (s *Settings) validateHTTPTimeout() error {
	return validatePositiveSeconds("http_timeout", s.HTTPTimeout)
}

// validatePositiveInt checks that an optional setting, if set, is positive.
func validatePositiveInt(name string, value *int) error {
	if value != nil && *value <= 0 {
		return fmt.Errorf("%w: %s: must be a positive number", ErrSettingInvalidParameter, name)
	}

	return nil
}

// validateHTTPMaxIdleConnsPerHost checks that the max idle conns per host, if set, is positive.
func (s *Settings) validateHTTPMaxIdleConnsPerHost() error {
	return validatePositiveInt("http_max_idle_conns_per_host", s.HTTPMaxIdleConnsPerHost)
}

// validateProxmoxAPIRetryAttempts checks that the retry attempts count, if set, is positive.
func (s *Settings) validateProxmoxAPIRetryAttempts() error {
	return validatePositiveInt("proxmox_api_retry_attempts", s.ProxmoxAPIRetryAttempts)
}

// getDiskMaxIndex returns the maximum valid index for a given disk type.
func getDiskMaxIndex(diskType string) int {
	switch diskType {
	case "ide": //nolint:goconst
		return maxIDEIndex
	case "scsi":
		return maxSCSIIndex
	case "sata":
		return maxSATAIndex
	case "virtio":
		return maxVIRTIOIndex
	default:
		return 0
	}
}

// validateInstanceAutoresizeSize checks that the autoresize size setting is valid.
func (s *Settings) validateInstanceAutoresizeSize() error {
	if s.InstanceAutoresizeSize == "" {
		return nil
	}

	matched, err := regexp.MatchString(`^\+?\d+(\.\d+)?[KMGT]?$`, s.InstanceAutoresizeSize)
	if err != nil {
		return fmt.Errorf("unable to compile regex: %w", err)
	}

	if !matched {
		return fmt.Errorf("%w: instance_autoresize_size: must be a valid absolute size, or a size increment (e.g.: +10G or 512M)", ErrSettingInvalidParameter)
	}

	return nil
}

// validateInstanceAutoresizeConsistency checks that disk and size settings are consistent.
func (s *Settings) validateInstanceAutoresizeConsistency() error {
	if s.InstanceAutoresizeDisk != "" && s.InstanceAutoresizeSize == "" {
		return fmt.Errorf("%w: instance_autoresize_size must have a value when instance_autoresize_disk is set", ErrSettingInvalidParameter)
	}

	if s.InstanceAutoresizeDisk == "" && s.InstanceAutoresizeSize != "" {
		return fmt.Errorf("%w: instance_autoresize_disk must have a value when instance_autoresize_size is set", ErrSettingInvalidParameter)
	}

	return nil
}
