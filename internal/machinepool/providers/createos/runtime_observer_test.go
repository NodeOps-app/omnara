package createos

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/omnara-ai/omnara/internal/machinepool/providers"
	"github.com/omnara-ai/omnara/internal/storage"
	"github.com/stretchr/testify/require"
)

func TestCreateOSObserveRuntimeStatesNormalizesEachTarget(t *testing.T) {
	running, runningSandbox := runtimeTargetAndSandbox(t, "sb-running", sandboxStatusRunning)
	inactive, inactiveSandbox := runtimeTargetAndSandbox(t, "sb-paused", sandboxStatusPaused)
	transitional, transitionalSandbox := runtimeTargetAndSandbox(t, "sb-resuming", sandboxStatusResuming)
	terminated, terminatedSandbox := runtimeTargetAndSandbox(t, "sb-destroyed", sandboxStatusDestroyed)
	missing, _ := runtimeTargetAndSandbox(t, "sb-missing", sandboxStatusRunning)
	foreign, foreignSandbox := runtimeTargetAndSandbox(t, "sb-foreign", sandboxStatusRunning)
	foreignSandbox.Name = "someone-else"
	mismatched, mismatchedSandbox := runtimeTargetAndSandbox(t, "sb-expected", sandboxStatusRunning)
	mismatchedSandbox.ID = "sb-different"
	unknown, unknownSandbox := runtimeTargetAndSandbox(t, "sb-unknown", "future_status")
	invalid := providers.RuntimeTarget{}

	api := newFakeAPI()
	api.getOverrides = map[string]sandbox{mismatched.ProviderResourceID: mismatchedSandbox}
	for _, current := range []sandbox{
		runningSandbox,
		inactiveSandbox,
		transitionalSandbox,
		terminatedSandbox,
		foreignSandbox,
		unknownSandbox,
	} {
		api.withSandbox(current)
	}
	targets := []providers.RuntimeTarget{
		running,
		inactive,
		transitional,
		terminated,
		missing,
		foreign,
		mismatched,
		unknown,
		invalid,
	}
	wantStates := []providers.RuntimeState{
		providers.RuntimeStateRunning,
		providers.RuntimeStateInactive,
		providers.RuntimeStateTransitional,
		providers.RuntimeStateTerminated,
		providers.RuntimeStateTerminated,
		providers.RuntimeStateUnknown,
		providers.RuntimeStateUnknown,
		providers.RuntimeStateUnknown,
		providers.RuntimeStateUnknown,
	}

	observations, err := newTestProvider(api).ObserveRuntimeStates(context.Background(), targets)
	if err != nil {
		t.Fatalf("observe createos runtimes: %v", err)
	}
	if len(observations) != len(targets) {
		t.Fatalf("observations = %d, want %d", len(observations), len(targets))
	}
	for index, observation := range observations {
		if observation.MachineID != targets[index].MachineID ||
			observation.ProviderResourceID != targets[index].ProviderResourceID ||
			observation.State != wantStates[index] {
			t.Fatalf(
				"observation %d = %+v, want state %q for %+v",
				index,
				observation,
				wantStates[index],
				targets[index],
			)
		}
	}
	// The invalid target must not reach the provider API.
	if api.getCalls != len(targets)-1 {
		t.Fatalf("sandbox lookups = %d, want %d", api.getCalls, len(targets)-1)
	}
}

func TestCreateOSObserveRuntimeStatesDoesNotReadWithoutValidTargets(t *testing.T) {
	installationID := uuid.New()
	machineID := uuid.New()
	tests := []struct {
		name   string
		target providers.RuntimeTarget
	}{
		{name: "empty"},
		{
			name:   "no installation",
			target: providers.RuntimeTarget{MachineID: machineID, ProviderResourceID: "sb-1"},
		},
		{
			name:   "no machine",
			target: providers.RuntimeTarget{InstallationID: installationID, ProviderResourceID: "sb-1"},
		},
		{
			name:   "no resource id",
			target: providers.RuntimeTarget{InstallationID: installationID, MachineID: machineID},
		},
		{
			name: "untrimmed resource id",
			target: providers.RuntimeTarget{
				InstallationID:     installationID,
				MachineID:          machineID,
				ProviderResourceID: " sb-1 ",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI()
			observations, err := newTestProvider(api).ObserveRuntimeStates(
				context.Background(),
				[]providers.RuntimeTarget{tt.target},
			)
			if err != nil {
				t.Fatalf("observe invalid createos runtime target: %v", err)
			}
			if len(observations) != 1 || observations[0].State != providers.RuntimeStateUnknown {
				t.Fatalf("observations = %+v, want one unknown observation", observations)
			}
			if api.getCalls != 0 {
				t.Fatalf("sandbox lookups = %d, want none", api.getCalls)
			}
		})
	}
}

func TestCreateOSObserveRuntimeStateUsesFreshExactRead(t *testing.T) {
	t.Run("running and owned", func(t *testing.T) {
		target, current := runtimeTargetAndSandbox(t, "sb-1", sandboxStatusRunning)
		api := newFakeAPI().withSandbox(current)
		observation, err := newTestProvider(api).ObserveRuntimeState(context.Background(), target)
		if err != nil || observation.State != providers.RuntimeStateRunning {
			t.Fatalf("observation = %+v, error %v", observation, err)
		}
		if len(api.getLookups) != 1 || api.getLookups[0] != target.ProviderResourceID {
			t.Fatalf("sandbox lookups = %#v", api.getLookups)
		}
	})

	t.Run("not found is terminated", func(t *testing.T) {
		target, _ := runtimeTargetAndSandbox(t, "sb-missing", sandboxStatusRunning)
		api := newFakeAPI()
		api.missingIDs = map[string]bool{target.ProviderResourceID: true}
		observation, err := newTestProvider(api).ObserveRuntimeState(context.Background(), target)
		if err != nil || observation.State != providers.RuntimeStateTerminated {
			t.Fatalf("observation = %+v, error %v", observation, err)
		}
	})

	t.Run("ownership mismatch is unknown", func(t *testing.T) {
		target, current := runtimeTargetAndSandbox(t, "sb-foreign", sandboxStatusRunning)
		current.Name = "someone-else"
		api := newFakeAPI().withSandbox(current)
		observation, err := newTestProvider(api).ObserveRuntimeState(context.Background(), target)
		if err != nil || observation.State != providers.RuntimeStateUnknown {
			t.Fatalf("observation = %+v, error %v", observation, err)
		}
	})

	t.Run("resource id mismatch is unknown", func(t *testing.T) {
		target, current := runtimeTargetAndSandbox(t, "sb-expected", sandboxStatusRunning)
		current.ID = "sb-different"
		api := newFakeAPI()
		api.getOverrides = map[string]sandbox{target.ProviderResourceID: current}
		observation, err := newTestProvider(api).ObserveRuntimeState(context.Background(), target)
		if err != nil || observation.State != providers.RuntimeStateUnknown {
			t.Fatalf("observation = %+v, error %v", observation, err)
		}
	})

	t.Run("provider error fails open", func(t *testing.T) {
		target, _ := runtimeTargetAndSandbox(t, "sb-errored", sandboxStatusRunning)
		api := newFakeAPI()
		api.getErr = apiError{StatusCode: http.StatusServiceUnavailable}
		observation, err := newTestProvider(api).ObserveRuntimeState(context.Background(), target)
		if err == nil || observation.State != providers.RuntimeStateUnknown {
			t.Fatalf("observation = %+v, error %v", observation, err)
		}
		if observation.MachineID != target.MachineID ||
			observation.ProviderResourceID != target.ProviderResourceID {
			t.Fatalf("errored observation lost its target identity: %+v", observation)
		}
	})
}

func TestCreateOSObserveRuntimeStatesFailsOnProviderError(t *testing.T) {
	target, _ := runtimeTargetAndSandbox(t, "sb-1", sandboxStatusRunning)
	api := newFakeAPI()
	api.getErr = apiError{StatusCode: http.StatusServiceUnavailable}
	observations, err := newTestProvider(api).ObserveRuntimeStates(
		context.Background(),
		[]providers.RuntimeTarget{target},
	)
	if err == nil || observations != nil {
		t.Fatalf("observations = %+v, error %v", observations, err)
	}
}

func TestCreateOSRuntimeStateAllowlist(t *testing.T) {
	tests := map[sandboxStatus]providers.RuntimeState{
		sandboxStatusRunning:    providers.RuntimeStateRunning,
		" RUNNING ":             providers.RuntimeStateRunning,
		sandboxStatusPaused:     providers.RuntimeStateInactive,
		sandboxStatusCreating:   providers.RuntimeStateTransitional,
		sandboxStatusPausing:    providers.RuntimeStateTransitional,
		sandboxStatusResuming:   providers.RuntimeStateTransitional,
		sandboxStatusForking:    providers.RuntimeStateTransitional,
		sandboxStatusDestroying: providers.RuntimeStateTransitional,
		sandboxStatusDestroyed:  providers.RuntimeStateTerminated,
		sandboxStatusFailed:     providers.RuntimeStateTerminated,
		sandboxStatusError:      providers.RuntimeStateUnknown,
		"future_status":         providers.RuntimeStateUnknown,
		"":                      providers.RuntimeStateUnknown,
	}
	for input, want := range tests {
		t.Run(string(input), func(t *testing.T) {
			got := createOSRuntimeState(input)
			if got != want {
				t.Fatalf("state %q = %q, want %q", input, got, want)
			}
			if !got.Valid() {
				t.Fatalf("state %q mapped to invalid runtime state %q", input, got)
			}
		})
	}
}

func runtimeTargetAndSandbox(
	t *testing.T,
	resourceID string,
	status sandboxStatus,
) (providers.RuntimeTarget, sandbox) {
	t.Helper()
	target := providers.RuntimeTarget{
		InstallationID:     uuid.New(),
		MachineID:          uuid.New(),
		ProviderResourceID: resourceID,
	}
	require.NotEqual(t, storage.NilID, target.InstallationID)
	expectedName, err := allocationName(target.InstallationID, target.MachineID)
	require.NoError(t, err)
	return target, sandbox{
		ID:     resourceID,
		Name:   expectedName,
		Status: status,
		Shape:  "2vcpu-4gb",
		RootFS: "ubuntu-24-04",
		Region: "eu-central",
	}
}
