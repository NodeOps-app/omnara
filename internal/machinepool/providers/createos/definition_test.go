package createos

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/omnara-ai/omnara/internal/machinepool/providers"
	"github.com/omnara-ai/omnara/internal/storage/executionstore"
)

func TestParseCreateOSProviderConfigDefaults(t *testing.T) {
	config, err := parseProviderConfig(nil)
	if err != nil {
		t.Fatalf("parse empty createos provider config: %v", err)
	}
	if config.APIBaseURL != defaultAPIBaseURL || config.AllowedShapes != nil ||
		config.AllowedRootFSes != nil || config.AllowedRegions != nil {
		t.Fatalf("default createos provider config = %+v", config)
	}
	explicitNull, err := parseProviderConfig(json.RawMessage(`null`))
	if err != nil || explicitNull.APIBaseURL != defaultAPIBaseURL || explicitNull.AllowedShapes != nil {
		t.Fatalf("null createos provider config = %+v error %v", explicitNull, err)
	}
}

func TestParseCreateOSProviderConfigNormalizesBaseURL(t *testing.T) {
	config, err := parseProviderConfig(json.RawMessage(`{"api_base_url":"  https://api.example.test/  "}`))
	if err != nil {
		t.Fatalf("parse createos provider config: %v", err)
	}
	if config.APIBaseURL != "https://api.example.test" {
		t.Fatalf("normalized base url = %q", config.APIBaseURL)
	}
}

func TestParseCreateOSProviderConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "unknown field", raw: `{"nope":true}`, want: "decode createos provider config"},
		{name: "trailing value", raw: `{} {}`, want: "decode createos provider config"},
		{name: "relative base url", raw: `{"api_base_url":"/v1"}`, want: "must be an absolute URL"},
		{name: "insecure base url", raw: `{"api_base_url":"http://api.example.test"}`, want: "must use https"},
		{
			name: "base url query",
			raw:  `{"api_base_url":"https://api.example.test?x=1"}`,
			want: "must not include query or fragment",
		},
		{
			name: "base url fragment",
			raw:  `{"api_base_url":"https://api.example.test#f"}`,
			want: "must not include query or fragment",
		},
		{name: "empty shape allowlist", raw: `{"allowed_shapes":[]}`, want: "allowed_shapes must not be empty"},
		{name: "invalid shape", raw: `{"allowed_shapes":["Not A Label"]}`, want: "allowed_shapes[0]"},
		{name: "mixed shape wildcard", raw: `{"allowed_shapes":["*","2vcpu-4gb"]}`, want: "cannot mix wildcard"},
		{name: "empty rootfs allowlist", raw: `{"allowed_rootfses":[]}`, want: "allowed_rootfses must not be empty"},
		{name: "blank rootfs", raw: `{"allowed_rootfses":[" "]}`, want: "allowed_rootfses[0]"},
		{name: "empty region allowlist", raw: `{"allowed_regions":[]}`, want: "allowed_regions must not be empty"},
		{name: "invalid region", raw: `{"allowed_regions":["-eu"]}`, want: "allowed_regions[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseProviderConfig(json.RawMessage(tt.raw))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseCreateOSProviderOptions(t *testing.T) {
	options, err := parseProviderOptions(testOptions(t, "  2vcpu-4gb ", " ubuntu-24-04 ", " eu-central ", "echo ready"))
	if err != nil {
		t.Fatalf("parse createos provider options: %v", err)
	}
	if options.Shape != "2vcpu-4gb" || options.RootFS != "ubuntu-24-04" ||
		options.Region != "eu-central" || options.StartupScript != "echo ready" {
		t.Fatalf("parsed createos provider options = %+v", options)
	}
	if _, err := parseProviderOptions(map[string]json.RawMessage{
		"shape":  mustRawJSON(t, "2vcpu-4gb"),
		"region": mustRawJSON(t, "eu-central"),
	}); err != nil {
		t.Fatalf("parse createos provider options without rootfs: %v", err)
	}
}

func TestParseCreateOSProviderOptionsRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		options map[string]json.RawMessage
		want    string
	}{
		{name: "missing options", options: nil, want: "requires provider_options"},
		{
			name:    "unknown option",
			options: map[string]json.RawMessage{"shape": mustRawJSON(t, "2vcpu-4gb"), "nope": mustRawJSON(t, "x")},
			want:    "decode createos provider_options",
		},
		{
			name:    "wrong option type",
			options: map[string]json.RawMessage{"shape": mustRawJSON(t, 1)},
			want:    "decode createos provider_options",
		},
		{name: "missing shape", options: testOptions(t, "", "ubuntu-24-04", "eu-central", ""), want: "shape:"},
		{name: "invalid shape", options: testOptions(t, "Big Shape", "ubuntu-24-04", "eu-central", ""), want: "shape:"},
		{name: "wildcard rootfs", options: testOptions(t, "2vcpu-4gb", "*", "eu-central", ""), want: "rootfs:"},
		{name: "missing region", options: testOptions(t, "2vcpu-4gb", "ubuntu-24-04", "", ""), want: "region:"},
		{name: "invalid region", options: testOptions(t, "2vcpu-4gb", "ubuntu-24-04", "eu_central", ""), want: "region:"},
		{
			name:    "oversized startup script",
			options: testOptions(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", strings.Repeat("a", 64*1024+1)),
			want:    "startup_script must be at most",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseProviderOptions(tt.options)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCreateOSNewProviderRequiresAuthToken(t *testing.T) {
	if _, err := (Definition{}).NewProvider(nil, providers.RuntimeConfig{
		OmnaraAPIURL: "https://api.omnara.test/v1",
	}); err == nil || !strings.Contains(err.Error(), "auth token is required") {
		t.Fatalf("missing token error = %v", err)
	}
	if _, err := (Definition{}).NewRuntimeProvider(nil, providers.RuntimeConfig{
		OmnaraAPIURL:      "https://api.omnara.test/v1",
		ProviderAuthToken: "   ",
	}); err == nil || !strings.Contains(err.Error(), "auth token is required") {
		t.Fatalf("blank runtime token error = %v", err)
	}
}

func TestCreateOSNewProviderUsesConfiguredBaseURL(t *testing.T) {
	machineProvider, err := (Definition{}).NewProvider(
		json.RawMessage(`{"api_base_url":"https://api.example.test"}`),
		providers.RuntimeConfig{OmnaraAPIURL: "https://api.omnara.test/v1", ProviderAuthToken: "token"},
	)
	if err != nil {
		t.Fatalf("new createos provider: %v", err)
	}
	concrete, ok := machineProvider.(*provider)
	if !ok {
		t.Fatal("createos provider has an unexpected implementation")
	}
	rest, ok := concrete.api.(*restClient)
	if !ok || rest.baseURL != "https://api.example.test" || rest.token != "token" {
		t.Fatalf("createos api client = %+v", concrete.api)
	}
	if concrete.omnaraAPIURL != "https://api.omnara.test/v1" {
		t.Fatalf("omnara api url = %q", concrete.omnaraAPIURL)
	}
	if _, err := (Definition{}).NewProvider(
		json.RawMessage(`{"api_base_url":"http://api.example.test"}`),
		providers.RuntimeConfig{OmnaraAPIURL: "https://api.omnara.test/v1", ProviderAuthToken: "token"},
	); err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("insecure base url error = %v", err)
	}
}

func TestCreateOSValidatePoolEnforcesResourceContract(t *testing.T) {
	if err := (Definition{}).ValidatePool(createOSPolicyForTest(t, nil)); err != nil {
		t.Fatalf("validate createos pool: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*executionstore.MachinePoolProviderPolicy)
		want   string
	}{
		{
			name:   "requires total cpu limit",
			mutate: func(policy *executionstore.MachinePoolProviderPolicy) { policy.ResourceLimits.MaxTotalCPU = nil },
			want:   "createos machine pools require max_total_cpu",
		},
		{
			name:   "requires machine memory limit",
			mutate: func(policy *executionstore.MachinePoolProviderPolicy) { policy.ResourceLimits.MaxMachineMemoryMB = nil },
			want:   "createos machine pools require max_machine_memory_mb",
		},
		{
			name: "rejects invalid default cpu",
			mutate: func(policy *executionstore.MachinePoolProviderPolicy) {
				zero := 0
				policy.DefaultProvisioning.CPU = &zero
			},
			want: "createos default_machine_cpu must be positive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := createOSPolicyForTest(t, nil)
			tt.mutate(&policy)
			err := (Definition{}).ValidatePool(policy)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCreateOSValidatePoolAcceptsOptionalResourceDefaults(t *testing.T) {
	policy := createOSPolicyForTest(t, nil)
	cpu := 2
	memoryMB := 4096
	policy.DefaultProvisioning.CPU = &cpu
	policy.DefaultProvisioning.MemoryMB = &memoryMB
	if err := (Definition{}).ValidatePool(policy); err != nil {
		t.Fatalf("validate createos pool with resource defaults: %v", err)
	}
}

func TestCreateOSValidateMachineProvisioningEnforcesAllowlists(t *testing.T) {
	config := json.RawMessage(
		`{"allowed_shapes":["2vcpu-4gb"],"allowed_rootfses":["ubuntu-24-04"],"allowed_regions":["eu-central"]}`,
	)
	tests := []struct {
		name    string
		options map[string]json.RawMessage
		want    string
	}{
		{
			name:    "disallowed shape",
			options: testOptions(t, "8vcpu-16gb", "ubuntu-24-04", "eu-central", ""),
			want:    "provider_config.allowed_shapes",
		},
		{
			name:    "disallowed rootfs",
			options: testOptions(t, "2vcpu-4gb", "debian-13", "eu-central", ""),
			want:    "provider_config.allowed_rootfses",
		},
		{
			name:    "disallowed region",
			options: testOptions(t, "2vcpu-4gb", "ubuntu-24-04", "us-east", ""),
			want:    "provider_config.allowed_regions",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (Definition{}).ValidateMachineProvisioning(
				createOSPolicyForTest(t, config),
				executionstore.MachineProvisioningConfig{ProviderOptions: tt.options},
			)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
	if err := (Definition{}).ValidateMachineProvisioning(
		createOSPolicyForTest(t, config),
		executionstore.MachineProvisioningConfig{
			ProviderOptions: testOptions(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
		},
	); err != nil {
		t.Fatalf("validate allowed createos machine provisioning: %v", err)
	}
}

func TestCreateOSValidateMachineProvisioningFallsBackToPoolDefaults(t *testing.T) {
	policy := createOSPolicyForTest(t, nil)
	if err := (Definition{}).ValidateMachineProvisioning(
		policy,
		executionstore.MachineProvisioningConfig{
			ProviderOptions: testOptions(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
		},
	); err != nil {
		t.Fatalf("validate createos machine provisioning matching pool defaults: %v", err)
	}
	err := (Definition{}).ValidateMachineProvisioning(
		policy,
		executionstore.MachineProvisioningConfig{
			ProviderOptions: testOptions(t, "8vcpu-16gb", "ubuntu-24-04", "eu-central", ""),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "provider_config.allowed_shapes") {
		t.Fatalf("shape outside pool default error = %v", err)
	}
}

func TestCreateOSBuildIntentLeavesShapeResourcesUnresolved(t *testing.T) {
	cpu := 2
	memoryMB := 4096
	intent, err := (Definition{}).BuildMachineProvisioningIntent(
		createOSPolicyForTest(t, json.RawMessage(`{"allowed_shapes":["*"]}`)),
		executionstore.MachineProvisioningConfig{
			CPU:             &cpu,
			MemoryMB:        &memoryMB,
			ProviderOptions: testOptions(t, "8vcpu-16gb", "ubuntu-24-04", "eu-central", ""),
		},
	)
	if err != nil {
		t.Fatalf("build createos provisioning intent: %v", err)
	}
	if intent.CPU != nil || intent.MemoryMB != nil {
		t.Fatalf("createos intent resources = cpu %v memory %v, want unresolved", intent.CPU, intent.MemoryMB)
	}
	if len(intent.ProviderOptions) != 4 {
		t.Fatalf("createos intent provider options = %#v", intent.ProviderOptions)
	}
	if _, err := (Definition{}).BuildMachineProvisioningIntent(
		createOSPolicyForTest(t, nil),
		executionstore.MachineProvisioningConfig{
			ProviderOptions: testOptions(t, "8vcpu-16gb", "ubuntu-24-04", "eu-central", ""),
		},
	); err == nil {
		t.Fatal("build createos intent accepted a disallowed shape")
	}
}

func TestCreateOSResolveMachineProviderOptionsPrefersMostSpecific(t *testing.T) {
	resolved := (Definition{}).ResolveMachineProviderOptions(
		testOptions(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
		map[string]json.RawMessage{"region": mustRawJSON(t, "us-east")},
		map[string]json.RawMessage{"shape": mustRawJSON(t, "8vcpu-16gb")},
	)
	options, err := parseProviderOptions(resolved)
	if err != nil {
		t.Fatalf("parse resolved createos provider options: %v", err)
	}
	if options.Shape != "8vcpu-16gb" || options.Region != "us-east" || options.RootFS != "ubuntu-24-04" {
		t.Fatalf("resolved createos provider options = %+v", options)
	}
}

func TestCreateOSPrepareProvisioningResolvesShapeResources(t *testing.T) {
	api := newFakeAPI()
	facts, err := newTestProvider(api).PrepareProvisioning(
		context.Background(),
		testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
	)
	if err != nil {
		t.Fatalf("prepare createos provisioning: %v", err)
	}
	if facts.CPU == nil || *facts.CPU != 2 || facts.MemoryMB == nil || *facts.MemoryMB != 4096 {
		t.Fatalf("resolved resources = cpu %v memory %v", facts.CPU, facts.MemoryMB)
	}
}

func TestCreateOSPrepareProvisioningKeepsConfiguredResources(t *testing.T) {
	api := newFakeAPI()
	cpu := 8
	memoryMB := 16384
	provisioning := testMachineProvisioning(t, "8vcpu-16gb", "ubuntu-24-04", "eu-central", "")
	provisioning.CPU = &cpu
	provisioning.MemoryMB = &memoryMB
	facts, err := newTestProvider(api).PrepareProvisioning(context.Background(), provisioning)
	if err != nil {
		t.Fatalf("prepare createos provisioning: %v", err)
	}
	if facts.CPU != &cpu || facts.MemoryMB != &memoryMB {
		t.Fatalf("resolved resources = cpu %v memory %v", facts.CPU, facts.MemoryMB)
	}
	if len(api.shapes) != 1 || api.shapesErr != nil {
		t.Fatalf("shape catalog was mutated: %+v", api.shapes)
	}
}

func TestCreateOSPrepareProvisioningRejectsUnusableShapes(t *testing.T) {
	tests := []struct {
		name   string
		shapes []Shape
		want   string
	}{
		{
			name:   "unknown shape",
			shapes: []Shape{{ID: "8vcpu-16gb", VCPU: 8, MemMiB: 16384}},
			want:   `shape "2vcpu-4gb" was not found`,
		},
		{name: "no shapes", shapes: nil, want: `shape "2vcpu-4gb" was not found`},
		{name: "zero cpu", shapes: []Shape{{ID: "2vcpu-4gb", MemMiB: 4096}}, want: "invalid resources"},
		{name: "zero memory", shapes: []Shape{{ID: "2vcpu-4gb", VCPU: 2}}, want: "invalid resources"},
		{name: "negative memory", shapes: []Shape{{ID: "2vcpu-4gb", VCPU: 2, MemMiB: -1}}, want: "invalid resources"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI()
			api.shapes = tt.shapes
			_, err := newTestProvider(api).PrepareProvisioning(
				context.Background(),
				testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
			)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCreateOSPrepareProvisioningSurfacesCatalogFailures(t *testing.T) {
	api := newFakeAPI()
	api.shapesErr = apiError{StatusCode: 503}
	_, err := newTestProvider(api).PrepareProvisioning(
		context.Background(),
		testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
	)
	if err == nil || !strings.Contains(err.Error(), "list createos shapes") {
		t.Fatalf("catalog failure error = %v", err)
	}
	if _, err := newTestProvider(newFakeAPI()).PrepareProvisioning(
		context.Background(),
		executionstore.MachineProvisioningConfig{},
	); err == nil || !strings.Contains(err.Error(), "requires provider_options") {
		t.Fatalf("missing provider options error = %v", err)
	}
}

func createOSPolicyForTest(
	t *testing.T,
	providerConfig json.RawMessage,
) executionstore.MachinePoolProviderPolicy {
	t.Helper()
	maxCPU := 8
	maxMemoryMB := 16384
	return executionstore.MachinePoolProviderPolicy{
		DefaultProvisioning: executionstore.MachineProvisioningConfig{
			ProviderOptions: testOptions(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
		},
		ResourceLimits: executionstore.MachineResourceLimits{
			MaxTotalCPU:        &maxCPU,
			MaxTotalMemoryMB:   &maxMemoryMB,
			MaxMachineCPU:      &maxCPU,
			MaxMachineMemoryMB: &maxMemoryMB,
		},
		ProviderConfig: providerConfig,
	}
}
