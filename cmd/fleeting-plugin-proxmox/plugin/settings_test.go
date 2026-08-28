package plugin

import (
	"testing"

	"github.com/stretchr/testify/require"
)

//nolint:gochecknoglobals
var (
	sampleURL = "https://example.com"
	//nolint:gosec
	sampleCredentialsPath      = "/tmp/proxmox_credentials.json"
	samplePool                 = "sample_pool"
	sampleStorage              = "sample_storage"
	sampleTemplateID           = 20
	sampleMaxInstances         = 7
	sampleInstanceNameCreating = "proxmox-creating"
	sampleInstanceNameRunning  = "running-prox"
	sampleInstanceNameRemoving = "proxve-removing"
	sampleInvalidTimeout       = 0
)

func TestSettings_fillWithDefaults(t *testing.T) {
	settings := Settings{}
	settings.FillWithDefaults()

	// Don't use consts here, we want to ensure they are not changed
	require.False(t, settings.InsecureSkipTLSVerify)
	require.Equal(t, "fleeting-creating", settings.InstanceNameCreating)
	require.Equal(t, "fleeting-running", settings.InstanceNameRunning)
	require.Equal(t, "fleeting-removing", settings.InstanceNameRemoving)
	require.Equal(t, "ipv4", settings.InstanceNetworkProtocol)
	require.Equal(t, 10, *settings.ProxmoxTaskWaitInterval)
	require.Equal(t, 300, *settings.ProxmoxTaskWaitTimeout)
	require.Equal(t, 120, *settings.InstanceAgentStartTimeout)
	require.Equal(t, 60, *settings.InstanceConnectTimeout)
	require.Equal(t, 60, *settings.CollectorInterval)
	require.Equal(t, 60, *settings.HTTPTimeout)
	require.Equal(t, 8, *settings.HTTPMaxIdleConnsPerHost)
	require.Equal(t, 3, *settings.ProxmoxAPIRetryAttempts)

	settings2 := Settings{
		InstanceNameCreating: sampleInstanceNameCreating,
		InstanceNameRunning:  sampleInstanceNameRunning,
		InstanceNameRemoving: sampleInstanceNameRemoving,
	}
	settings2.FillWithDefaults()

	require.Equal(t, sampleInstanceNameCreating, settings2.InstanceNameCreating)
	require.Equal(t, sampleInstanceNameRunning, settings2.InstanceNameRunning)
	require.Equal(t, sampleInstanceNameRemoving, settings2.InstanceNameRemoving)
}

func TestSettings_fillWithDefaults_timeoutsHonoured(t *testing.T) {
	taskWaitTimeout := 111
	agentStartTimeout := 222
	connectTimeout := 333
	collectorInterval := 444
	httpTimeout := 555
	httpMaxIdleConnsPerHost := 16
	proxmoxAPIRetryAttempts := 5

	settings := Settings{
		ProxmoxTaskWaitTimeout:    &taskWaitTimeout,
		InstanceAgentStartTimeout: &agentStartTimeout,
		InstanceConnectTimeout:    &connectTimeout,
		CollectorInterval:         &collectorInterval,
		HTTPTimeout:               &httpTimeout,
		HTTPMaxIdleConnsPerHost:   &httpMaxIdleConnsPerHost,
		ProxmoxAPIRetryAttempts:   &proxmoxAPIRetryAttempts,
	}
	settings.FillWithDefaults()

	require.Equal(t, taskWaitTimeout, *settings.ProxmoxTaskWaitTimeout)
	require.Equal(t, agentStartTimeout, *settings.InstanceAgentStartTimeout)
	require.Equal(t, connectTimeout, *settings.InstanceConnectTimeout)
	require.Equal(t, collectorInterval, *settings.CollectorInterval)
	require.Equal(t, httpTimeout, *settings.HTTPTimeout)
	require.Equal(t, httpMaxIdleConnsPerHost, *settings.HTTPMaxIdleConnsPerHost)
	require.Equal(t, proxmoxAPIRetryAttempts, *settings.ProxmoxAPIRetryAttempts)
}

func TestSettings_validateTimeoutSettings(t *testing.T) {
	negative := -1
	zero := 0
	positive := 30

	tests := []struct {
		name          string
		value         *int
		expectedError error
	}{
		{"unset is valid", nil, nil},
		{"positive is valid", &positive, nil},
		{"zero is invalid", &zero, ErrSettingInvalidParameter},
		{"negative is invalid", &negative, ErrSettingInvalidParameter},
	}

	fields := []struct {
		name     string
		setValue func(*Settings, *int)
		validate func(*Settings) error
	}{
		{
			"proxmox_task_wait_timeout",
			func(s *Settings, v *int) { s.ProxmoxTaskWaitTimeout = v },
			(*Settings).validateProxmoxTaskWaitTimeout,
		},
		{
			"instance_agent_start_timeout",
			func(s *Settings, v *int) { s.InstanceAgentStartTimeout = v },
			(*Settings).validateInstanceAgentStartTimeout,
		},
		{
			"instance_connect_timeout",
			func(s *Settings, v *int) { s.InstanceConnectTimeout = v },
			(*Settings).validateInstanceConnectTimeout,
		},
		{
			"collector_interval",
			func(s *Settings, v *int) { s.CollectorInterval = v },
			(*Settings).validateCollectorInterval,
		},
		{
			"http_timeout",
			func(s *Settings, v *int) { s.HTTPTimeout = v },
			(*Settings).validateHTTPTimeout,
		},
	}

	for _, field := range fields {
		for _, tt := range tests {
			t.Run(field.name+"/"+tt.name, func(t *testing.T) {
				settings := &Settings{}
				field.setValue(settings, tt.value)

				err := field.validate(settings)
				require.ErrorIs(t, err, tt.expectedError)
			})
		}
	}
}

func TestSettings_validateHTTPMaxIdleConnsPerHost(t *testing.T) {
	negative := -1
	zero := 0
	positive := 16

	tests := []struct {
		name          string
		value         *int
		expectedError error
	}{
		{"unset is valid", nil, nil},
		{"positive is valid", &positive, nil},
		{"zero is invalid", &zero, ErrSettingInvalidParameter},
		{"negative is invalid", &negative, ErrSettingInvalidParameter},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := &Settings{HTTPMaxIdleConnsPerHost: tt.value}

			err := settings.validateHTTPMaxIdleConnsPerHost()
			require.ErrorIs(t, err, tt.expectedError)
		})
	}
}

func TestSettings_validateProxmoxAPIRetryAttempts(t *testing.T) {
	negative := -1
	zero := 0
	positive := 5

	tests := []struct {
		name          string
		value         *int
		expectedError error
	}{
		{"unset is valid", nil, nil},
		{"positive is valid", &positive, nil},
		{"zero is invalid", &zero, ErrSettingInvalidParameter},
		{"negative is invalid", &negative, ErrSettingInvalidParameter},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := &Settings{ProxmoxAPIRetryAttempts: tt.value}

			err := settings.validateProxmoxAPIRetryAttempts()
			require.ErrorIs(t, err, tt.expectedError)
		})
	}
}

func TestSettings_validateVMIDRange(t *testing.T) {
	var (
		templateID = 20
		start100   = 100
		end200     = 200
		end100     = 100 // equal to start100, an empty range
		start20    = 20  // equal to templateID
		end21      = 21
	)

	tests := []struct {
		name          string
		start, end    *int
		expectedError error
	}{
		{"both unset is valid", nil, nil, nil},
		{"both set, start < end, excludes template is valid", &start100, &end200, nil},
		{"only start set is invalid", &start100, nil, ErrSettingInvalidParameter},
		{"only end set is invalid", nil, &end200, ErrSettingInvalidParameter},
		{"start == end is invalid", &start100, &end100, ErrSettingInvalidParameter},
		{"start > end is invalid", &end200, &start100, ErrSettingInvalidParameter},
		{"range containing template_id is invalid", &start20, &end21, ErrSettingInvalidParameter},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := &Settings{
				TemplateID:     &templateID,
				VMIDRangeStart: tt.start,
				VMIDRangeEnd:   tt.end,
			}

			err := settings.validateVMIDRange()
			require.ErrorIs(t, err, tt.expectedError)
		})
	}
}

func TestSettings_checkRequiredFields(t *testing.T) {
	tests := []struct {
		name          string
		settings      Settings
		expectedError error
	}{
		{
			name: "Missing URL",
			settings: Settings{
				CredentialsFilePath: sampleCredentialsPath,
				Pool:                samplePool,
				Storage:             sampleStorage,
				TemplateID:          &sampleTemplateID,
				MaxInstances:        &sampleMaxInstances,
			},
			expectedError: ErrRequiredSettingMissing,
		},
		{
			name: "Missing credentials file path",
			settings: Settings{
				URL:          sampleURL,
				Pool:         samplePool,
				Storage:      sampleStorage,
				TemplateID:   &sampleTemplateID,
				MaxInstances: &sampleMaxInstances,
			},
			expectedError: ErrRequiredSettingMissing,
		},
		{
			name: "Missing pool",
			settings: Settings{
				URL:                 sampleURL,
				CredentialsFilePath: sampleCredentialsPath,
				Storage:             sampleStorage,
				TemplateID:          &sampleTemplateID,
				MaxInstances:        &sampleMaxInstances,
			},
			expectedError: ErrRequiredSettingMissing,
		},
		{
			name: "Missing storage",
			settings: Settings{
				URL:                 sampleURL,
				CredentialsFilePath: sampleCredentialsPath,
				Pool:                samplePool,
				TemplateID:          &sampleTemplateID,
				MaxInstances:        &sampleMaxInstances,
			},
			expectedError: nil,
		},
		{
			name: "Missing template id",
			settings: Settings{
				URL:                 sampleURL,
				CredentialsFilePath: sampleCredentialsPath,
				Pool:                samplePool,
				Storage:             sampleStorage,
				TemplateID:          nil,
				MaxInstances:        &sampleMaxInstances,
			},
			expectedError: ErrRequiredSettingMissing,
		},
		{
			name: "Missing max instances",
			settings: Settings{
				URL:                 sampleURL,
				CredentialsFilePath: sampleCredentialsPath,
				Pool:                samplePool,
				Storage:             sampleStorage,
				TemplateID:          nil,
				MaxInstances:        nil,
			},
			expectedError: ErrRequiredSettingMissing,
		},
		{
			name: "No missing parameters",
			settings: Settings{
				URL:                 sampleURL,
				CredentialsFilePath: sampleCredentialsPath,
				Pool:                samplePool,
				Storage:             sampleStorage,
				TemplateID:          &sampleTemplateID,
				MaxInstances:        &sampleMaxInstances,
			},
			expectedError: nil,
		},
		{
			name: "Invalid protocol",
			settings: Settings{
				URL:                     sampleURL,
				CredentialsFilePath:     sampleCredentialsPath,
				Pool:                    samplePool,
				Storage:                 sampleStorage,
				TemplateID:              &sampleTemplateID,
				MaxInstances:            &sampleMaxInstances,
				InstanceNetworkProtocol: "invalid-protocol",
			},
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name: "Invalid collector interval",
			settings: Settings{
				URL:                 sampleURL,
				CredentialsFilePath: sampleCredentialsPath,
				Pool:                samplePool,
				Storage:             sampleStorage,
				TemplateID:          &sampleTemplateID,
				MaxInstances:        &sampleMaxInstances,
				CollectorInterval:   &sampleInvalidTimeout,
			},
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name: "Invalid http timeout",
			settings: Settings{
				URL:                 sampleURL,
				CredentialsFilePath: sampleCredentialsPath,
				Pool:                samplePool,
				Storage:             sampleStorage,
				TemplateID:          &sampleTemplateID,
				MaxInstances:        &sampleMaxInstances,
				HTTPTimeout:         &sampleInvalidTimeout,
			},
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name: "Invalid http max idle conns per host",
			settings: Settings{
				URL:                     sampleURL,
				CredentialsFilePath:     sampleCredentialsPath,
				Pool:                    samplePool,
				Storage:                 sampleStorage,
				TemplateID:              &sampleTemplateID,
				MaxInstances:            &sampleMaxInstances,
				HTTPMaxIdleConnsPerHost: &sampleInvalidTimeout,
			},
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name: "Invalid proxmox api retry attempts",
			settings: Settings{
				URL:                     sampleURL,
				CredentialsFilePath:     sampleCredentialsPath,
				Pool:                    samplePool,
				Storage:                 sampleStorage,
				TemplateID:              &sampleTemplateID,
				MaxInstances:            &sampleMaxInstances,
				ProxmoxAPIRetryAttempts: &sampleInvalidTimeout,
			},
			expectedError: ErrSettingInvalidParameter,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.settings.CheckRequiredFields()
			require.ErrorIs(t, err, tt.expectedError)
		})
	}
}

func TestSettings_validateInstanceAutoresizeDisk(t *testing.T) {
	tests := []struct {
		name          string
		disk          string
		expectedError error
	}{
		{
			name:          "Empty disk is valid",
			disk:          "",
			expectedError: nil,
		},
		{
			name:          "Valid ide0",
			disk:          "ide0",
			expectedError: nil,
		},
		{
			name:          "Valid ide3",
			disk:          "ide3",
			expectedError: nil,
		},
		{
			name:          "Valid scsi0",
			disk:          "scsi0",
			expectedError: nil,
		},
		{
			name:          "Valid scsi29",
			disk:          "scsi29",
			expectedError: nil,
		},
		{
			name:          "Valid sata0",
			disk:          "sata0",
			expectedError: nil,
		},
		{
			name:          "Valid sata4",
			disk:          "sata4",
			expectedError: nil,
		},
		{
			name:          "Invalid ide4 - index exceeds max",
			disk:          "ide4",
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name:          "Invalid scsi31 - index exceeds max",
			disk:          "scsi31",
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name:          "Invalid sata6 - index exceeds max",
			disk:          "sata6",
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name:          "Invalid virtio16 - index exceeds max",
			disk:          "virtio16",
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name:          "Invalid disk format",
			disk:          "nvme0",
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name:          "Invalid disk format no index",
			disk:          "ide",
			expectedError: ErrSettingInvalidParameter,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := Settings{
				InstanceAutoresizeDisk: tt.disk,
			}
			err := settings.validateInstanceAutoresizeDisk()
			require.ErrorIs(t, err, tt.expectedError)
		})
	}
}

func TestSettings_validateInstanceAutoresizeSize(t *testing.T) {
	tests := []struct {
		name          string
		size          string
		expectedError error
	}{
		{
			name:          "Empty size is valid",
			size:          "",
			expectedError: nil,
		},
		{
			name:          "Valid absolute size no unit",
			size:          "10",
			expectedError: nil,
		},
		{
			name:          "Valid absolute size 10G",
			size:          "10G",
			expectedError: nil,
		},
		{
			name:          "Valid absolute size 512M",
			size:          "512M",
			expectedError: nil,
		},
		{
			name:          "Valid absolute size 1T",
			size:          "1T",
			expectedError: nil,
		},
		{
			name:          "Valid absolute size 1K",
			size:          "1K",
			expectedError: nil,
		},
		{
			name:          "Valid increment +10G",
			size:          "+10G",
			expectedError: nil,
		},
		{
			name:          "Valid increment +512M",
			size:          "+512M",
			expectedError: nil,
		},
		{
			name:          "Valid decimal size 1.5G",
			size:          "1.5G",
			expectedError: nil,
		},
		{
			name:          "Valid decimal increment +1.5G",
			size:          "+1.5G",
			expectedError: nil,
		},
		{
			name:          "Invalid size negative",
			size:          "-10G",
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name:          "Invalid size invalid unit",
			size:          "10P",
			expectedError: ErrSettingInvalidParameter,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := Settings{
				InstanceAutoresizeSize: tt.size,
			}
			err := settings.validateInstanceAutoresizeSize()
			require.ErrorIs(t, err, tt.expectedError)
		})
	}
}

func TestSettings_validateInstanceAutoresizeConsistency(t *testing.T) {
	tests := []struct {
		name          string
		disk          string
		size          string
		expectedError error
	}{
		{
			name:          "Both empty is valid",
			disk:          "",
			size:          "",
			expectedError: nil,
		},
		{
			name:          "Both set is valid",
			disk:          "ide0",
			size:          "10G",
			expectedError: nil,
		},
		{
			name:          "Disk without size is invalid",
			disk:          "ide0",
			size:          "",
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name:          "Size without disk is invalid",
			disk:          "",
			size:          "10G",
			expectedError: ErrSettingInvalidParameter,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := Settings{
				InstanceAutoresizeDisk: tt.disk,
				InstanceAutoresizeSize: tt.size,
			}
			err := settings.validateInstanceAutoresizeConsistency()
			require.ErrorIs(t, err, tt.expectedError)
		})
	}
}

func TestGetDiskMaxIndex(t *testing.T) {
	tests := []struct {
		name     string
		diskType string
		expected int
	}{
		{
			name:     "ide returns maxIDEIndex",
			diskType: "ide",
			expected: maxIDEIndex,
		},
		{
			name:     "scsi returns maxSCSIIndex",
			diskType: "scsi",
			expected: maxSCSIIndex,
		},
		{
			name:     "sata returns maxSATAIndex",
			diskType: "sata",
			expected: maxSATAIndex,
		},
		{
			name:     "unknown returns 0",
			diskType: "nvme",
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, getDiskMaxIndex(tt.diskType))
		})
	}
}

func TestSettings_CheckRequiredFields_fullValidation(t *testing.T) {
	tests := []struct {
		name          string
		settings      Settings
		expectedError error
	}{
		{
			name: "Valid autoresize disk without size passes disk validation but fails consistency",
			settings: Settings{
				URL:                    sampleURL,
				CredentialsFilePath:    sampleCredentialsPath,
				Pool:                   samplePool,
				TemplateID:             &sampleTemplateID,
				MaxInstances:           &sampleMaxInstances,
				InstanceAutoresizeDisk: "ide0",
			},
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name: "Valid autoresize with both disk and size passes",
			settings: Settings{
				URL:                    sampleURL,
				CredentialsFilePath:    sampleCredentialsPath,
				Pool:                   samplePool,
				TemplateID:             &sampleTemplateID,
				MaxInstances:           &sampleMaxInstances,
				InstanceAutoresizeDisk: "ide0",
				InstanceAutoresizeSize: "10G",
			},
			expectedError: nil,
		},
		{
			name: "Invalid autoresize disk format",
			settings: Settings{
				URL:                    sampleURL,
				CredentialsFilePath:    sampleCredentialsPath,
				Pool:                   samplePool,
				TemplateID:             &sampleTemplateID,
				MaxInstances:           &sampleMaxInstances,
				InstanceAutoresizeDisk: "nvme0",
				InstanceAutoresizeSize: "10G",
			},
			expectedError: ErrSettingInvalidParameter,
		},
		{
			name: "Invalid autoresize size format",
			settings: Settings{
				URL:                    sampleURL,
				CredentialsFilePath:    sampleCredentialsPath,
				Pool:                   samplePool,
				TemplateID:             &sampleTemplateID,
				MaxInstances:           &sampleMaxInstances,
				InstanceAutoresizeDisk: "ide0",
				InstanceAutoresizeSize: "invalid",
			},
			expectedError: ErrSettingInvalidParameter,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.settings.CheckRequiredFields()
			require.ErrorIs(t, err, tt.expectedError)
		})
	}
}
