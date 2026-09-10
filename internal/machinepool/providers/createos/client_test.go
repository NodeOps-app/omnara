package createos

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omnara-ai/omnara/internal/machinepool/providers"
)

func TestCreateOSRESTClientReadsCatalogsSandboxesAndProcesses(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-token" {
			t.Errorf("api key header = %q", r.Header.Get("X-Api-Key"))
			http.Error(w, "test handler failed", http.StatusInternalServerError)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/shapes":
			_, _ = w.Write([]byte(`{"data":[{"id":"2vcpu-4gb","vcpu":2,"mem_mib":4096}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/rootfs":
			_, _ = w.Write([]byte(
				`{"status":"ok","data":{"rootfs":["ubuntu-24-04"],"default":"ubuntu-24-04",` +
					`"entries":[{"name":"ubuntu-24-04","deprecated":false}]}}`,
			))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes":
			if r.URL.RawQuery != "limit=500&offset=1000" {
				t.Errorf("list query = %q", r.URL.RawQuery)
				http.Error(w, "test handler failed", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(
				`{"data":[{"id":"sb-1","name":"omnara-abc","status":"running","shape":"2vcpu-4gb",` +
					`"rootfs":"ubuntu-24-04","region":"eu-central"}],"pagination":{"total":1001}}`,
			))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var request createSandboxRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode create sandbox request: %v", err)
				http.Error(w, "test handler failed", http.StatusInternalServerError)
				return
			}
			if request.Shape != "2vcpu-4gb" || request.RootFS != "ubuntu-24-04" ||
				request.Name != "omnara-abc" || request.Region != "eu-central" ||
				request.Envs["TEST"] != "value" {
				t.Errorf("create sandbox request = %+v", request)
				http.Error(w, "test handler failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":"ok","data":{"id":"sb-1","name":"omnara-abc"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-1":
			_, _ = w.Write([]byte(
				`{"status":"ok","data":{"id":"sb-1","name":"omnara-abc","status":"paused",` +
					`"shape":"2vcpu-4gb","rootfs":"ubuntu-24-04","region":"eu-central"}}`,
			))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sb-1/resume":
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/sb-1":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sb-1/processes":
			_, _ = w.Write([]byte(`{"processes":[{"process_id":"p-1","state":"running","leader_exited":false}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/sb-1/processes":
			var request createProcessRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode create process request: %v", err)
				http.Error(w, "test handler failed", http.StatusInternalServerError)
				return
			}
			if request.Command != "/bin/sh" || len(request.Args) != 2 || request.Args[0] != "-c" {
				t.Errorf("create process request = %+v", request)
				http.Error(w, "test handler failed", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"process_id":"p-2","state":"starting","leader_exited":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newRESTClient(server.URL, "test-token", server.Client())
	ctx := context.Background()

	shapes, err := client.ListShapes(ctx)
	if err != nil || len(shapes) != 1 || shapes[0].ID != "2vcpu-4gb" ||
		shapes[0].VCPU != 2 || shapes[0].MemMiB != 4096 {
		t.Fatalf("list shapes = %+v error %v", shapes, err)
	}

	catalog, err := client.ListRootFS(ctx)
	if err != nil || catalog.Default != "ubuntu-24-04" || len(catalog.Names) != 1 ||
		len(catalog.Entries) != 1 || catalog.Entries[0].Name != "ubuntu-24-04" {
		t.Fatalf("list rootfs = %+v error %v", catalog, err)
	}

	items, total, err := client.ListSandboxes(ctx, 500, 1000)
	if err != nil || total != 1001 || len(items) != 1 || items[0].ID != "sb-1" ||
		items[0].Status != sandboxStatusRunning || items[0].Region != "eu-central" {
		t.Fatalf("list sandboxes = %+v total %d error %v", items, total, err)
	}

	created, err := client.CreateSandbox(ctx, createSandboxRequest{
		Shape:  "2vcpu-4gb",
		RootFS: "ubuntu-24-04",
		Name:   "omnara-abc",
		Region: "eu-central",
		Envs:   map[string]string{"TEST": "value"},
	})
	if err != nil || created.ID != "sb-1" || created.Name != "omnara-abc" {
		t.Fatalf("create sandbox = %+v error %v", created, err)
	}

	fetched, found, err := client.GetSandbox(ctx, "sb-1")
	if err != nil || !found || fetched.Status != sandboxStatusPaused || fetched.Shape != "2vcpu-4gb" {
		t.Fatalf("get sandbox = %+v found %v error %v", fetched, found, err)
	}

	if err := client.ResumeSandbox(ctx, "sb-1"); err != nil {
		t.Fatalf("resume sandbox: %v", err)
	}

	processes, err := client.ListProcesses(ctx, "sb-1")
	if err != nil || len(processes) != 1 || processes[0].ID != "p-1" || processes[0].State != "running" {
		t.Fatalf("list processes = %+v error %v", processes, err)
	}

	startedProcess, err := client.CreateProcess(ctx, "sb-1", createProcessRequest{
		Command: "/bin/sh",
		Args:    []string{"-c", "true"},
	})
	if err != nil || startedProcess.ID != "p-2" || startedProcess.State != "starting" {
		t.Fatalf("create process = %+v error %v", startedProcess, err)
	}

	if err := client.DeleteSandbox(ctx, "sb-1"); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
}

func TestCreateOSRESTClientTreatsMissingSandboxAsAbsent(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := newRESTClient(server.URL, "token", server.Client())
	target, found, err := client.GetSandbox(context.Background(), "sb-missing")
	if err != nil || found || target.ID != "" {
		t.Fatalf("get missing sandbox = %+v found %v error %v", target, found, err)
	}
	if err := client.DeleteSandbox(context.Background(), "sb-missing"); err != nil {
		t.Fatalf("delete missing sandbox: %v", err)
	}
	if err := client.ResumeSandbox(context.Background(), "sb-missing"); !isNotFound(err) {
		t.Fatalf("resume missing sandbox = %v, want a not-found API error", err)
	}
}

func TestCreateOSRESTClientClassifiesHTTPFailures(t *testing.T) {
	for _, statusCode := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusConflict,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(statusCode)
			}))
			defer server.Close()
			client := newRESTClient(server.URL, "token", server.Client())
			_, err := client.ListShapes(context.Background())
			var apiErr apiError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != statusCode {
				t.Fatalf("list shapes error = %v, want API error %d", err, statusCode)
			}
			if !strings.Contains(err.Error(), "createos API returned HTTP") {
				t.Fatalf("error message = %q", err.Error())
			}
			if isNotFound(err) {
				t.Fatalf("status %d was classified as not found", statusCode)
			}
		})
	}
}

func TestCreateOSRESTClientPreservesRetryAfterHint(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter string
		want       time.Duration
		hinted     bool
	}{
		{name: "seconds", retryAfter: "7", want: 7 * time.Second, hinted: true},
		{name: "absent"},
		{name: "unparseable", retryAfter: "soon"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()
			_, _, err := newRESTClient(server.URL, "token", server.Client()).
				ListSandboxes(context.Background(), 500, 0)
			delay, hinted := providers.RetryAfter(err)
			if hinted != tt.hinted || delay != tt.want {
				t.Fatalf("retry-after = %v hinted %v, want %v hinted %v", delay, hinted, tt.want, tt.hinted)
			}
			if !errors.As(err, &apiError{}) {
				t.Fatalf("error = %v, want an API error", err)
			}
		})
	}
}

func TestCreateOSRESTClientRejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "invalid json", body: `{`, want: "decode createos response"},
		{
			name: "wrong data type",
			body: `{"status":"ok","data":{"rootfs":"not-a-list"}}`,
			want: "decode createos response data",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			_, err := newRESTClient(server.URL, "token", server.Client()).ListRootFS(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCreateOSRESTClientAcceptsEmptyResponsePayloads(t *testing.T) {
	for _, body := range []string{"", `{"status":"ok","data":null}`, `{"status":"ok"}`} {
		t.Run("body "+body, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			catalog, err := newRESTClient(server.URL, "token", server.Client()).ListRootFS(context.Background())
			if err != nil || catalog.Default != "" || catalog.Names != nil {
				t.Fatalf("catalog = %+v error %v", catalog, err)
			}
		})
	}
}

func TestCreateOSRESTClientEscapesSandboxIDsInPaths(t *testing.T) {
	var requested string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	_, _, err := newRESTClient(server.URL, "token", server.Client()).
		GetSandbox(context.Background(), "sb/../admin")
	if err != nil {
		t.Fatalf("get sandbox with a traversal id: %v", err)
	}
	if requested != "/v1/sandboxes/sb%2F..%2Fadmin" {
		t.Fatalf("requested path = %q", requested)
	}
}

func TestNewCreateOSRESTClientDefaultsHTTPClient(t *testing.T) {
	client := newRESTClient(defaultAPIBaseURL, "token", nil)
	if client.httpClient == nil {
		t.Fatal("rest client has no HTTP client")
	}
	if client.baseURL != defaultAPIBaseURL || client.token != "token" {
		t.Fatalf("rest client = %+v", client)
	}
}
