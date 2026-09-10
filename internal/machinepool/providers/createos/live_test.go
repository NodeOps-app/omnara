package createos

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/omnara-ai/omnara/internal/machinepool/providers"
	"github.com/omnara-ai/omnara/internal/storage/executionstore"
	"github.com/omnara-ai/omnara/internal/testutil/providercontract"
)

func TestCreateOSProviderLiveSmoke(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("CREATEOS_API_KEY"))
	if token == "" {
		t.Skip("a CreateOS API key is required")
	}
	region := strings.TrimSpace(os.Getenv("OMNARA_CREATEOS_TEST_REGION"))
	if region == "" {
		region = "us"
	}
	catalogCtx, catalogCancel := context.WithTimeout(context.Background(), 30*time.Second)
	shapeName := strings.TrimSpace(os.Getenv("OMNARA_CREATEOS_TEST_SHAPE"))
	if shapeName == "" {
		shapeName = smallestLiveShape(t, catalogCtx, token)
	}
	rootfsName := strings.TrimSpace(os.Getenv("OMNARA_CREATEOS_TEST_ROOTFS"))
	if rootfsName == "" {
		catalog, err := ListRootFS(catalogCtx, token)
		if err != nil {
			t.Fatalf("list live createos rootfs: %v", err)
		}
		rootfsName = catalog.Default
	}
	catalogCancel()
	if shapeName == "" || rootfsName == "" {
		t.Fatalf("live createos catalog is empty: shape %q rootfs %q", shapeName, rootfsName)
	}

	config := mustRawJSON(t, map[string]any{
		"allowed_shapes":   []string{"*"},
		"allowed_rootfses": []string{"*"},
		"allowed_regions":  []string{"*"},
	})
	provisional := executionstore.MachineProvisioningConfig{
		ProviderOptions: testOptions(t, shapeName, rootfsName, region, ""),
	}
	omnaraPublicURL := strings.TrimSpace(os.Getenv("OMNARA_PUBLIC_URL"))
	if omnaraPublicURL == "" {
		omnaraPublicURL = "https://app.omnara.com"
	}
	omnaraPublicAPIURL := strings.TrimSpace(os.Getenv("OMNARA_PUBLIC_API_URL"))
	if omnaraPublicAPIURL == "" {
		omnaraPublicAPIURL = omnaraPublicURL + "/api/v1"
	}
	machineProvider, err := (Definition{}).NewProvider(
		config,
		providers.RuntimeConfig{
			OmnaraAPIURL:      omnaraPublicAPIURL,
			ProviderAuthToken: token,
		},
	)
	if err != nil {
		t.Fatalf("new live createos provider: %v", err)
	}
	concreteProvider, ok := machineProvider.(*provider)
	if !ok {
		t.Fatal("CreateOS provider has an unexpected implementation")
	}
	concreteProvider.api = liveTestAPI{apiClient: concreteProvider.api}

	facts, err := machineProvider.PrepareProvisioning(context.Background(), provisional)
	if err != nil {
		t.Fatalf("prepare live createos provisioning: %v", err)
	}
	if facts.CPU == nil || *facts.CPU <= 0 || facts.MemoryMB == nil || *facts.MemoryMB <= 0 {
		t.Fatalf("live createos shape resources = cpu %v memory %v", facts.CPU, facts.MemoryMB)
	}
	provisioning := provisional
	installationID := uuid.New()
	machineID := uuid.New()
	var cleanupResourceID string
	t.Cleanup(func() {
		if cleanupResourceID == "" {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := machineProvider.DeleteMachine(
			cleanupCtx,
			installationID,
			machineID,
			provisioning,
			cleanupResourceID,
		); err != nil {
			t.Errorf("delete live createos sandbox: %v", err)
		}
	})
	provisionCtx, provisionCancel := context.WithTimeout(context.Background(), provisioningTimeout)
	result, err := machineProvider.ProvisionMachine(
		provisionCtx,
		installationID,
		machineID,
		provisioning,
		"live-smoke-token",
		nil,
	)
	provisionCancel()
	cleanupResourceID = result.ProviderResourceID
	if err != nil {
		t.Fatalf("provision live createos sandbox: %v", err)
	}

	reprovisionCtx, reprovisionCancel := context.WithTimeout(context.Background(), provisioningTimeout)
	reprovisioned, err := machineProvider.ProvisionMachine(
		reprovisionCtx,
		installationID,
		machineID,
		provisioning,
		"live-smoke-token",
		nil,
	)
	reprovisionCancel()
	if err != nil {
		t.Fatalf("reprovision live createos sandbox: %v", err)
	}
	if reprovisioned.ProviderResourceID != result.ProviderResourceID {
		t.Fatalf(
			"reprovisioned resource id = %q, want %q",
			reprovisioned.ProviderResourceID,
			result.ProviderResourceID,
		)
	}

	inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer inspectCancel()
	inspected, found, err := machineProvider.InspectMachine(
		inspectCtx,
		installationID,
		machineID,
		provisioning,
		result.ProviderResourceID,
	)
	if err != nil || !found || inspected != result.ProviderResourceID {
		t.Fatalf("inspect live createos sandbox = %q found %v error %v", inspected, found, err)
	}

	observer, ok := machineProvider.(providers.RuntimeStateObserver)
	if !ok {
		t.Fatal("createos provider does not implement runtime observation")
	}
	runtimeTarget := providers.RuntimeTarget{
		InstallationID:      installationID,
		MachineID:           machineID,
		ProviderResourceID:  result.ProviderResourceID,
		MachineProvisioning: provisioning,
	}
	observationCtx, observationCancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer observationCancel()
	providercontract.WaitForPresentRuntimeObservation(
		t,
		observationCtx,
		runtimeTarget,
		func() (providers.RuntimeObservation, error) {
			return observer.ObserveRuntimeState(observationCtx, runtimeTarget)
		},
	)
	providercontract.WaitForPresentRuntimeObservation(
		t,
		observationCtx,
		runtimeTarget,
		func() (providers.RuntimeObservation, error) {
			observations, err := observer.ObserveRuntimeStates(
				observationCtx,
				[]providers.RuntimeTarget{runtimeTarget},
			)
			if err != nil {
				return providers.RuntimeObservation{}, err
			}
			if len(observations) != 1 {
				return providers.RuntimeObservation{}, fmt.Errorf(
					"bulk live CreateOS runtime observations = %d, want 1",
					len(observations),
				)
			}
			return observations[0], nil
		},
	)

	missingTarget := runtimeTarget
	missingTarget.MachineID = uuid.New()
	missingTarget.ProviderResourceID = "sb-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	missingCtx, missingCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer missingCancel()
	missingObservation, err := observer.ObserveRuntimeState(missingCtx, missingTarget)
	if err != nil {
		t.Fatalf("exact observe missing live CreateOS sandbox: %v", err)
	}
	providercontract.AssertRuntimeObservation(
		t,
		missingTarget,
		missingObservation,
		providers.RuntimeStateTerminated,
	)
}

func smallestLiveShape(t *testing.T, ctx context.Context, token string) string {
	t.Helper()
	shapes, err := ListShapes(ctx, token)
	if err != nil {
		t.Fatalf("list live createos shapes: %v", err)
	}
	usable := slices.DeleteFunc(shapes, func(candidate Shape) bool {
		return candidate.ID == "" || candidate.VCPU <= 0 || candidate.MemMiB <= 0
	})
	if len(usable) == 0 {
		t.Fatal("live createos account has no usable shapes")
	}
	slices.SortFunc(usable, func(a, b Shape) int {
		if a.MemMiB != b.MemMiB {
			return a.MemMiB - b.MemMiB
		}
		return a.VCPU - b.VCPU
	})
	return usable[0].ID
}

type liveTestAPI struct {
	apiClient
}

func (a liveTestAPI) CreateSandbox(
	ctx context.Context,
	request createSandboxRequest,
) (sandbox, error) {
	if request.Envs == nil {
		request.Envs = map[string]string{}
	}
	request.Envs[providercontract.LiveResourceEnv] = providercontract.LiveResourceValue
	return a.apiClient.CreateSandbox(ctx, request)
}
