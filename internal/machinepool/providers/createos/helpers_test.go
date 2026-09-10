package createos

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/omnara-ai/omnara/internal/storage/executionstore"
)

const testStartupScriptEnvVar = "OMNARA_STARTUP_SCRIPT_PAYLOAD"

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return raw
}

func testOptions(t *testing.T, shape, rootfs, region, startupScript string) map[string]json.RawMessage {
	t.Helper()
	return map[string]json.RawMessage{
		"shape":          mustRawJSON(t, shape),
		"rootfs":         mustRawJSON(t, rootfs),
		"region":         mustRawJSON(t, region),
		"startup_script": mustRawJSON(t, startupScript),
	}
}

func testMachineProvisioning(
	t *testing.T,
	shape, rootfs, region, startupScript string,
) executionstore.MachineProvisioningConfig {
	t.Helper()
	return executionstore.MachineProvisioningConfig{
		ProviderOptions: testOptions(t, shape, rootfs, region, startupScript),
	}
}

func newTestProvider(api apiClient) *provider {
	return &provider{
		api:          api,
		omnaraAPIURL: "https://api.omnara.test/v1",
	}
}

type listSandboxesCall struct {
	Limit  int
	Offset int
}

type fakeAPI struct {
	shapes    []Shape
	shapesErr error

	rootfs    RootFSCatalog
	rootfsErr error

	sandboxes []sandbox

	// createdID and createdStatus shape the sandbox the fake records for a create call.
	createdID     string
	createdStatus sandboxStatus
	createCalls   int
	createRequest createSandboxRequest
	createErr     error
	// racedSandboxes appear once a create attempt fails, modeling a concurrent attempt that won.
	racedSandboxes []sandbox

	listCalls   []listSandboxesCall
	listErr     error
	listErrCall int

	getCalls   int
	getLookups []string
	getErr     error
	missingIDs map[string]bool
	// getOverrides serves a sandbox that a lookup id would not otherwise resolve to.
	getOverrides map[string]sandbox

	deleteCalls int
	deletedIDs  []string
	deleteErr   error

	resumeCalls int
	resumedIDs  []string
	resumeErr   error

	processes            []process
	listProcessCalls     int
	listProcessLookups   []string
	listProcessesErr     error
	createProcessCalls   int
	createProcessRequest createProcessRequest
	createdProcessID     string
	createProcessErr     error
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		shapes:           []Shape{{ID: "2vcpu-4gb", VCPU: 2, MemMiB: 4096}},
		rootfs:           RootFSCatalog{Names: []string{"ubuntu-24-04"}, Default: "ubuntu-24-04"},
		createdID:        "sb-created",
		createdStatus:    sandboxStatusRunning,
		createdProcessID: "proc-1",
	}
}

// withSandbox registers a sandbox the fake serves from both list and get reads.
func (a *fakeAPI) withSandbox(target sandbox) *fakeAPI {
	a.sandboxes = append(a.sandboxes, target)
	return a
}

func (a *fakeAPI) ListShapes(context.Context) ([]Shape, error) {
	return a.shapes, a.shapesErr
}

func (a *fakeAPI) ListRootFS(context.Context) (RootFSCatalog, error) {
	return a.rootfs, a.rootfsErr
}

func (a *fakeAPI) CreateSandbox(_ context.Context, request createSandboxRequest) (sandbox, error) {
	a.createCalls++
	a.createRequest = request
	if a.createErr != nil {
		a.sandboxes = append(a.sandboxes, a.racedSandboxes...)
		a.racedSandboxes = nil
		return sandbox{}, a.createErr
	}
	created := sandbox{
		ID:     a.createdID,
		Name:   request.Name,
		Status: a.createdStatus,
		Shape:  request.Shape,
		RootFS: request.RootFS,
		Region: request.Region,
	}
	a.sandboxes = append(a.sandboxes, created)
	// The real create response omits lifecycle status and region.
	return sandbox{ID: created.ID, Name: created.Name, Shape: created.Shape, RootFS: created.RootFS}, nil
}

func (a *fakeAPI) ListSandboxes(_ context.Context, limit, offset int) ([]sandbox, int, error) {
	a.listCalls = append(a.listCalls, listSandboxesCall{Limit: limit, Offset: offset})
	if a.listErr != nil && (a.listErrCall == 0 || a.listErrCall == len(a.listCalls)) {
		return nil, 0, a.listErr
	}
	total := len(a.sandboxes)
	if offset >= total {
		return nil, total, nil
	}
	return slices.Clone(a.sandboxes[offset:min(offset+limit, total)]), total, nil
}

func (a *fakeAPI) GetSandbox(_ context.Context, id string) (sandbox, bool, error) {
	a.getCalls++
	a.getLookups = append(a.getLookups, id)
	if a.getErr != nil {
		return sandbox{}, false, a.getErr
	}
	if a.missingIDs[id] {
		return sandbox{}, false, nil
	}
	if override, ok := a.getOverrides[id]; ok {
		return override, true, nil
	}
	for _, candidate := range a.sandboxes {
		if candidate.ID == id {
			return candidate, true, nil
		}
	}
	return sandbox{}, false, nil
}

func (a *fakeAPI) DeleteSandbox(_ context.Context, id string) error {
	a.deleteCalls++
	a.deletedIDs = append(a.deletedIDs, id)
	return a.deleteErr
}

func (a *fakeAPI) ResumeSandbox(_ context.Context, id string) error {
	a.resumeCalls++
	a.resumedIDs = append(a.resumedIDs, id)
	return a.resumeErr
}

func (a *fakeAPI) ListProcesses(_ context.Context, id string) ([]process, error) {
	a.listProcessCalls++
	a.listProcessLookups = append(a.listProcessLookups, id)
	return a.processes, a.listProcessesErr
}

func (a *fakeAPI) CreateProcess(
	_ context.Context,
	_ string,
	request createProcessRequest,
) (process, error) {
	a.createProcessCalls++
	a.createProcessRequest = request
	if a.createProcessErr != nil {
		return process{}, a.createProcessErr
	}
	return process{ID: a.createdProcessID, State: "starting"}, nil
}

var _ apiClient = (*fakeAPI)(nil)
