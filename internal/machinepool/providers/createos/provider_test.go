package createos

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/omnara-ai/omnara/internal/machinepool/providers"
	"github.com/omnara-ai/omnara/internal/storage"
	"github.com/stretchr/testify/require"
)

func TestCreateOSProviderProvisionCreatesSandboxAndDaemonProcess(t *testing.T) {
	api := newFakeAPI()
	installationID := uuid.New()
	machineID := uuid.New()
	name := testAllocationName(t, installationID, machineID)
	result, err := newTestProvider(api).ProvisionMachine(
		context.Background(),
		installationID,
		machineID,
		testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", "echo ready"),
		"machine-token",
		nil,
	)
	if err != nil {
		t.Fatalf("provision createos sandbox: %v", err)
	}
	if result.ProviderResourceID != api.createdID {
		t.Fatalf("resource id = %q, want %q", result.ProviderResourceID, api.createdID)
	}
	if api.createRequest.Name != name || api.createRequest.Shape != "2vcpu-4gb" ||
		api.createRequest.RootFS != "ubuntu-24-04" || api.createRequest.Region != "eu-central" {
		t.Fatalf("create request = %+v", api.createRequest)
	}
	if api.createRequest.Envs["OMNARA_MACHINE_TOKEN"] != "machine-token" ||
		api.createRequest.Envs["OMNARA_API_URL"] != "https://api.omnara.test/v1" ||
		api.createRequest.Envs["OMNARA_INSTALLER_URL"] != "https://api.omnara.test/install/omnarad.sh" ||
		api.createRequest.Envs[testStartupScriptEnvVar] == "" ||
		api.createRequest.Envs[providers.ManagedBootstrapScriptEnvVar] != providers.ManagedBootScriptPayload() {
		t.Fatalf("managed env = %#v", api.createRequest.Envs)
	}
	// The create response omits status, so the provider must re-read the sandbox before adopting it.
	if api.getCalls != 1 || api.getLookups[0] != api.createdID {
		t.Fatalf("sandbox lookups = %#v", api.getLookups)
	}
	launcher := providers.ManagedDaemonLauncherArgs()
	if api.createProcessCalls != 1 || api.createProcessRequest.Command != launcher[0] ||
		len(api.createProcessRequest.Args) != len(launcher)-1 ||
		api.createProcessRequest.Args[0] != launcher[1] ||
		!strings.Contains(api.createProcessRequest.Args[1], providers.ManagedBootstrapScriptEnvVar) {
		t.Fatalf("daemon process request = %+v", api.createProcessRequest)
	}
}

func TestCreateOSProviderProvisionIncludesMachineEnv(t *testing.T) {
	api := newFakeAPI()
	_, err := newTestProvider(api).ProvisionMachine(
		context.Background(),
		uuid.New(),
		uuid.New(),
		testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
		"machine-token",
		map[string]string{"APP_ENV": "production", "GITHUB_TOKEN": "resolved-secret"},
	)
	if err != nil {
		t.Fatalf("provision createos sandbox: %v", err)
	}
	if api.createRequest.Envs["APP_ENV"] != "production" ||
		api.createRequest.Envs["GITHUB_TOKEN"] != "resolved-secret" {
		t.Fatalf("create env missing machine env: %#v", api.createRequest.Envs)
	}
	if api.createRequest.Envs[testStartupScriptEnvVar] != "" {
		t.Fatalf("create env carries a startup script payload: %#v", api.createRequest.Envs)
	}
	reserved := newFakeAPI()
	_, err = newTestProvider(reserved).ProvisionMachine(
		context.Background(),
		uuid.New(),
		uuid.New(),
		testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
		"machine-token",
		map[string]string{"OMNARA_API_URL": "https://attacker.test"},
	)
	if err == nil || !strings.Contains(err.Error(), "reserved OMNARA_ key") || reserved.createCalls != 0 {
		t.Fatalf("reserved env error = %v, create calls = %d", err, reserved.createCalls)
	}
}

func TestCreateOSProviderProvisionAdoptsExistingSandbox(t *testing.T) {
	installationID := uuid.New()
	machineID := uuid.New()
	name := testAllocationName(t, installationID, machineID)
	api := newFakeAPI().withSandbox(sandbox{
		ID:     "sb-existing",
		Name:   name,
		Status: sandboxStatusRunning,
		Shape:  "2vcpu-4gb",
		RootFS: "ubuntu-24-04",
		Region: "eu-central",
	})
	api.processes = []process{{ID: "p-1", State: "running"}}
	result, err := newTestProvider(api).ProvisionMachine(
		context.Background(),
		installationID,
		machineID,
		testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
		"machine-token",
		nil,
	)
	if err != nil || result.ProviderResourceID != "sb-existing" {
		t.Fatalf("adopt existing sandbox = %+v error %v", result, err)
	}
	if api.createCalls != 0 || api.getCalls != 0 || api.createProcessCalls != 0 {
		t.Fatalf(
			"adoption side effects = creates %d gets %d processes %d",
			api.createCalls,
			api.getCalls,
			api.createProcessCalls,
		)
	}
}

func TestCreateOSProviderProvisionRestartsDeadDaemonProcess(t *testing.T) {
	tests := []struct {
		name      string
		processes []process
		want      int
	}{
		{name: "no processes", want: 1},
		{name: "leader exited", processes: []process{{ID: "p-1", State: "running", LeaderExited: true}}, want: 1},
		{name: "exited", processes: []process{{ID: "p-1", State: "exited"}}, want: 1},
		{name: "starting", processes: []process{{ID: "p-1", State: "starting"}}},
		{name: "running", processes: []process{{ID: "p-1", State: "running"}}},
		{
			name:      "one live among dead",
			processes: []process{{ID: "p-1", State: "exited"}, {ID: "p-2", State: "running"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installationID := uuid.New()
			machineID := uuid.New()
			api := newFakeAPI().withSandbox(sandbox{
				ID:     "sb-existing",
				Name:   testAllocationName(t, installationID, machineID),
				Status: sandboxStatusRunning,
				Shape:  "2vcpu-4gb",
				RootFS: "ubuntu-24-04",
				Region: "eu-central",
			})
			api.processes = tt.processes
			if _, err := newTestProvider(api).ProvisionMachine(
				context.Background(),
				installationID,
				machineID,
				testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
				"machine-token",
				nil,
			); err != nil {
				t.Fatalf("provision createos sandbox: %v", err)
			}
			if api.createProcessCalls != tt.want {
				t.Fatalf("daemon process creations = %d, want %d", api.createProcessCalls, tt.want)
			}
		})
	}
}

func TestCreateOSProviderProvisionConvergesOnRacedCreate(t *testing.T) {
	installationID := uuid.New()
	machineID := uuid.New()
	name := testAllocationName(t, installationID, machineID)
	api := newFakeAPI()
	api.createErr = apiError{StatusCode: http.StatusConflict}
	api.racedSandboxes = []sandbox{{
		ID:     "sb-raced",
		Name:   name,
		Status: sandboxStatusRunning,
		Shape:  "2vcpu-4gb",
		RootFS: "ubuntu-24-04",
		Region: "eu-central",
	}}
	result, err := newTestProvider(api).ProvisionMachine(
		context.Background(),
		installationID,
		machineID,
		testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
		"machine-token",
		nil,
	)
	if err != nil || result.ProviderResourceID != "sb-raced" {
		t.Fatalf("raced provision = %+v error %v", result, err)
	}
	if api.createCalls != 1 || len(api.listCalls) != 2 || api.getCalls != 0 {
		t.Fatalf(
			"raced provision calls = creates %d lists %d gets %d",
			api.createCalls,
			len(api.listCalls),
			api.getCalls,
		)
	}
	if api.createProcessCalls != 1 {
		t.Fatalf("daemon process creations = %d, want 1", api.createProcessCalls)
	}
}

func TestCreateOSProviderProvisionSurfacesLookupFailures(t *testing.T) {
	api := newFakeAPI()
	api.listErr = apiError{StatusCode: http.StatusServiceUnavailable}
	result, err := newTestProvider(api).ProvisionMachine(
		context.Background(),
		uuid.New(),
		uuid.New(),
		testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
		"machine-token",
		nil,
	)
	if !errors.As(err, &apiError{}) || result.ProviderResourceID != "" || api.createCalls != 0 {
		t.Fatalf("lookup failure = %+v error %v creates %d", result, err, api.createCalls)
	}
}

func TestCreateOSProviderProvisionSurfacesCreateFailures(t *testing.T) {
	t.Run("create fails with no sandbox", func(t *testing.T) {
		api := newFakeAPI()
		api.createErr = apiError{StatusCode: http.StatusBadRequest}
		result, err := newTestProvider(api).ProvisionMachine(
			context.Background(),
			uuid.New(),
			uuid.New(),
			testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
			"machine-token",
			nil,
		)
		if !errors.As(err, &apiError{}) || result.ProviderResourceID != "" {
			t.Fatalf("create failure = %+v error %v", result, err)
		}
	})
	t.Run("created sandbox disappears", func(t *testing.T) {
		api := newFakeAPI()
		api.missingIDs = map[string]bool{api.createdID: true}
		_, err := newTestProvider(api).ProvisionMachine(
			context.Background(),
			uuid.New(),
			uuid.New(),
			testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
			"machine-token",
			nil,
		)
		if err == nil || !strings.Contains(err.Error(), "created createos sandbox was not found") {
			t.Fatalf("missing created sandbox error = %v", err)
		}
	})
	t.Run("read back fails but keeps the created id", func(t *testing.T) {
		api := newFakeAPI()
		api.getErr = apiError{StatusCode: http.StatusServiceUnavailable}
		result, err := newTestProvider(api).ProvisionMachine(
			context.Background(),
			uuid.New(),
			uuid.New(),
			testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
			"machine-token",
			nil,
		)
		if err == nil || result.ProviderResourceID != api.createdID {
			t.Fatalf("read-back failure = %+v error %v", result, err)
		}
	})
	t.Run("daemon process response has no id", func(t *testing.T) {
		api := newFakeAPI()
		api.createdProcessID = ""
		result, err := newTestProvider(api).ProvisionMachine(
			context.Background(),
			uuid.New(),
			uuid.New(),
			testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
			"machine-token",
			nil,
		)
		if err == nil || !strings.Contains(err.Error(), "missing process id") ||
			result.ProviderResourceID != api.createdID {
			t.Fatalf("process id error = %v result %+v", err, result)
		}
	})
	t.Run("daemon process listing fails", func(t *testing.T) {
		api := newFakeAPI()
		api.listProcessesErr = apiError{StatusCode: http.StatusServiceUnavailable}
		result, err := newTestProvider(api).ProvisionMachine(
			context.Background(),
			uuid.New(),
			uuid.New(),
			testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
			"machine-token",
			nil,
		)
		if !errors.As(err, &apiError{}) || result.ProviderResourceID != api.createdID ||
			api.createProcessCalls != 0 {
			t.Fatalf("process listing failure = %+v error %v", result, err)
		}
	})
}

func TestCreateOSProviderProvisionRejectsUnusableSandbox(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*sandbox)
		want    string
		options [4]string
	}{
		{
			name:   "wrong shape",
			mutate: func(target *sandbox) { target.Shape = "8vcpu-16gb" },
			want:   "does not match provisioning intent",
		},
		{
			name:   "wrong rootfs",
			mutate: func(target *sandbox) { target.RootFS = "debian-13" },
			want:   "does not match provisioning intent",
		},
		{
			name:   "wrong region",
			mutate: func(target *sandbox) { target.Region = "us-east" },
			want:   "does not match provisioning intent",
		},
		{
			name:   "paused",
			mutate: func(target *sandbox) { target.Status = sandboxStatusPaused },
			want:   "is not running",
		},
		{
			name:   "creating",
			mutate: func(target *sandbox) { target.Status = sandboxStatusCreating },
			want:   "is not running",
		},
		{
			name:   "blank id",
			mutate: func(target *sandbox) { target.ID = "" },
			want:   "does not match expected allocation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installationID := uuid.New()
			machineID := uuid.New()
			target := sandbox{
				ID:     "sb-existing",
				Name:   testAllocationName(t, installationID, machineID),
				Status: sandboxStatusRunning,
				Shape:  "2vcpu-4gb",
				RootFS: "ubuntu-24-04",
				Region: "eu-central",
			}
			tt.mutate(&target)
			api := newFakeAPI().withSandbox(target)
			result, err := newTestProvider(api).ProvisionMachine(
				context.Background(),
				installationID,
				machineID,
				testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
				"machine-token",
				nil,
			)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
			if api.createProcessCalls != 0 {
				t.Fatalf("started a daemon on an unusable sandbox")
			}
			if tt.name != "blank id" && result.ProviderResourceID != "sb-existing" {
				t.Fatalf("result id = %q, want the observed sandbox id", result.ProviderResourceID)
			}
		})
	}
}

func TestCreateOSProviderProvisionRejectsDuplicateAllocations(t *testing.T) {
	installationID := uuid.New()
	machineID := uuid.New()
	name := testAllocationName(t, installationID, machineID)
	api := newFakeAPI().
		withSandbox(sandbox{ID: "sb-a", Name: name, Status: sandboxStatusRunning}).
		withSandbox(sandbox{ID: "sb-b", Name: name, Status: sandboxStatusPaused})
	_, err := newTestProvider(api).ProvisionMachine(
		context.Background(),
		installationID,
		machineID,
		testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
		"machine-token",
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "multiple createos sandboxes have allocation name") {
		t.Fatalf("duplicate allocation error = %v", err)
	}
	if api.createCalls != 0 {
		t.Fatalf("created a sandbox despite duplicates")
	}
}

func TestCreateOSProviderFindByNameIgnoresDeadSandboxesAndPaginates(t *testing.T) {
	installationID := uuid.New()
	machineID := uuid.New()
	name := testAllocationName(t, installationID, machineID)
	api := newFakeAPI()
	// A destroyed and a failed sandbox share the allocation name and must not be adopted.
	api.withSandbox(sandbox{ID: "sb-destroyed", Name: name, Status: sandboxStatusDestroyed})
	api.withSandbox(sandbox{ID: "sb-failed", Name: name, Status: sandboxStatusFailed})
	for range 500 {
		api.withSandbox(sandbox{ID: uuid.NewString(), Name: "other", Status: sandboxStatusRunning})
	}
	api.withSandbox(sandbox{
		ID:     "sb-live",
		Name:   name,
		Status: sandboxStatusRunning,
		Shape:  "2vcpu-4gb",
		RootFS: "ubuntu-24-04",
		Region: "eu-central",
	})
	result, err := newTestProvider(api).ProvisionMachine(
		context.Background(),
		installationID,
		machineID,
		testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", ""),
		"machine-token",
		nil,
	)
	if err != nil || result.ProviderResourceID != "sb-live" {
		t.Fatalf("paginated lookup = %+v error %v", result, err)
	}
	if len(api.listCalls) != 2 || api.listCalls[0] != (listSandboxesCall{Limit: 500}) ||
		api.listCalls[1] != (listSandboxesCall{Limit: 500, Offset: 500}) {
		t.Fatalf("list calls = %#v", api.listCalls)
	}
	if api.createCalls != 0 {
		t.Fatalf("created a sandbox instead of adopting the live one")
	}
}

func TestCreateOSProviderInspectMachine(t *testing.T) {
	installationID := uuid.New()
	machineID := uuid.New()
	name := testAllocationName(t, installationID, machineID)
	provisioning := testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", "")

	t.Run("by resource id", func(t *testing.T) {
		api := newFakeAPI().withSandbox(sandbox{ID: "sb-1", Name: name, Status: sandboxStatusRunning})
		resourceID, found, err := newTestProvider(api).InspectMachine(
			context.Background(),
			installationID,
			machineID,
			provisioning,
			"sb-1",
		)
		if err != nil || !found || resourceID != "sb-1" || len(api.listCalls) != 0 {
			t.Fatalf("inspect = %q found %v error %v lists %d", resourceID, found, err, len(api.listCalls))
		}
	})

	t.Run("by allocation name", func(t *testing.T) {
		api := newFakeAPI().withSandbox(sandbox{ID: "sb-1", Name: name, Status: sandboxStatusPaused})
		resourceID, found, err := newTestProvider(api).InspectMachine(
			context.Background(),
			installationID,
			machineID,
			provisioning,
			"",
		)
		if err != nil || !found || resourceID != "sb-1" || api.getCalls != 0 {
			t.Fatalf("inspect = %q found %v error %v gets %d", resourceID, found, err, api.getCalls)
		}
	})

	t.Run("absent", func(t *testing.T) {
		api := newFakeAPI()
		api.missingIDs = map[string]bool{"sb-1": true}
		resourceID, found, err := newTestProvider(api).InspectMachine(
			context.Background(),
			installationID,
			machineID,
			provisioning,
			"sb-1",
		)
		if err != nil || found || resourceID != "" {
			t.Fatalf("inspect absent = %q found %v error %v", resourceID, found, err)
		}
	})

	t.Run("foreign sandbox", func(t *testing.T) {
		api := newFakeAPI().withSandbox(sandbox{ID: "sb-1", Name: "someone-else", Status: sandboxStatusRunning})
		_, found, err := newTestProvider(api).InspectMachine(
			context.Background(),
			installationID,
			machineID,
			provisioning,
			"sb-1",
		)
		if err == nil || found || !strings.Contains(err.Error(), "expected allocation name") {
			t.Fatalf("inspect foreign = found %v error %v", found, err)
		}
	})

	t.Run("provider failure", func(t *testing.T) {
		api := newFakeAPI()
		api.getErr = apiError{StatusCode: http.StatusServiceUnavailable}
		_, found, err := newTestProvider(api).InspectMachine(
			context.Background(),
			installationID,
			machineID,
			provisioning,
			"sb-1",
		)
		if !errors.As(err, &apiError{}) || found {
			t.Fatalf("inspect failure = found %v error %v", found, err)
		}
	})
}

func TestCreateOSProviderDeleteMachine(t *testing.T) {
	installationID := uuid.New()
	machineID := uuid.New()
	name := testAllocationName(t, installationID, machineID)
	provisioning := testMachineProvisioning(t, "2vcpu-4gb", "ubuntu-24-04", "eu-central", "")

	api := newFakeAPI().withSandbox(sandbox{ID: "sb-1", Name: name, Status: sandboxStatusRunning})
	machineProvider := newTestProvider(api)
	if err := machineProvider.DeleteMachine(
		context.Background(),
		installationID,
		machineID,
		provisioning,
		"sb-1",
	); err != nil {
		t.Fatalf("delete createos sandbox: %v", err)
	}
	if api.deleteCalls != 1 || api.deletedIDs[0] != "sb-1" {
		t.Fatalf("deleted ids = %#v", api.deletedIDs)
	}

	if err := machineProvider.DeleteMachine(
		context.Background(),
		installationID,
		machineID,
		provisioning,
		"",
	); err == nil || !strings.Contains(err.Error(), "provider resource id is required") {
		t.Fatalf("blank resource id error = %v", err)
	}
	if api.deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", api.deleteCalls)
	}

	api.missingIDs = map[string]bool{"sb-gone": true}
	if err := machineProvider.DeleteMachine(
		context.Background(),
		installationID,
		machineID,
		provisioning,
		"sb-gone",
	); err != nil {
		t.Fatalf("delete already absent sandbox: %v", err)
	}
	if api.deleteCalls != 1 {
		t.Fatalf("deleted an absent sandbox: %d calls", api.deleteCalls)
	}

	foreign := newFakeAPI().withSandbox(sandbox{ID: "sb-2", Name: "someone-else", Status: sandboxStatusRunning})
	if err := newTestProvider(foreign).DeleteMachine(
		context.Background(),
		installationID,
		machineID,
		provisioning,
		"sb-2",
	); err == nil || !strings.Contains(err.Error(), "expected allocation name") {
		t.Fatalf("foreign delete error = %v", err)
	}
	if foreign.deleteCalls != 0 {
		t.Fatalf("deleted a foreign sandbox")
	}
}

func TestCreateOSAllocationName(t *testing.T) {
	installationID := uuid.New()
	machineID := uuid.New()
	name, err := allocationName(installationID, machineID)
	require.NoError(t, err)
	if !strings.HasPrefix(name, "omnara-") || len(name) != len("omnara-")+15 {
		t.Fatalf("allocation name = %q", name)
	}
	if err := providers.ValidateDNSLabel(name); err != nil {
		t.Fatalf("allocation name %q is not a DNS label: %v", name, err)
	}
	repeated, err := allocationName(installationID, machineID)
	require.NoError(t, err)
	if repeated != name {
		t.Fatalf("allocation name is not deterministic: %q then %q", name, repeated)
	}
	other, err := allocationName(installationID, uuid.New())
	require.NoError(t, err)
	if other == name {
		t.Fatalf("two machines share allocation name %q", name)
	}
	if _, err := allocationName(storage.NilID, machineID); err == nil {
		t.Fatal("allocation name accepted a nil installation id")
	}
	if _, err := allocationName(installationID, storage.NilID); err == nil {
		t.Fatal("allocation name accepted a nil machine id")
	}
}

func TestCreateOSProviderWakeResumesSandboxByID(t *testing.T) {
	api := newFakeAPI()
	machineProvider := newTestProvider(api)
	input := providers.WakeMachineInput{ProviderResourceID: "sb-paused"}
	if err := machineProvider.WakeMachine(context.Background(), input); err != nil {
		t.Fatalf("wake createos machine: %v", err)
	}
	if api.resumeCalls != 1 || api.resumedIDs[0] != input.ProviderResourceID {
		t.Fatalf("resume calls = %d ids = %#v", api.resumeCalls, api.resumedIDs)
	}
	// A wake may be retried after an ambiguous transport failure.
	if err := machineProvider.WakeMachine(context.Background(), input); err != nil {
		t.Fatalf("repeat wake createos machine: %v", err)
	}
	if api.resumeCalls != 2 {
		t.Fatalf("repeat resume calls = %d, want 2", api.resumeCalls)
	}
}

func TestCreateOSProviderWakeRejectsMissingResourceID(t *testing.T) {
	api := newFakeAPI()
	err := newTestProvider(api).WakeMachine(context.Background(), providers.WakeMachineInput{
		SandboxURL: "https://sandbox.createos.test",
	})
	if err == nil || !strings.Contains(err.Error(), "provider resource id is required") {
		t.Fatalf("wake without a resource id = %v", err)
	}
	if api.resumeCalls != 0 {
		t.Fatalf("resume calls = %d, want none", api.resumeCalls)
	}
}

func TestCreateOSProviderWakeSurfacesProviderFailures(t *testing.T) {
	api := newFakeAPI()
	api.resumeErr = apiError{StatusCode: http.StatusServiceUnavailable}
	err := newTestProvider(api).WakeMachine(
		context.Background(),
		providers.WakeMachineInput{ProviderResourceID: "sb-paused"},
	)
	if !errors.As(err, &apiError{}) {
		t.Fatalf("wake failure = %v, want an API error", err)
	}
}

func TestCreateOSProviderProvisioningTimeout(t *testing.T) {
	if timeout := newTestProvider(newFakeAPI()).ProvisioningTimeout(); timeout != 2*time.Minute {
		t.Fatalf("provisioning timeout = %v", timeout)
	}
}

func testAllocationName(t *testing.T, installationID, machineID storage.ID) string {
	t.Helper()
	name, err := allocationName(installationID, machineID)
	require.NoError(t, err)
	return name
}
