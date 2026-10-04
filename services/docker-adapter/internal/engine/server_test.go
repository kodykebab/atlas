package engine

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frappe/atlas/services/docker-adapter/internal/atlas"
)

type harness struct {
	atlas   *fakeAtlas
	adapter *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFakeAtlas()
	atlasServer := httptest.NewServer(fake)
	t.Cleanup(atlasServer.Close)

	adapter := httptest.NewServer(NewServer(Config{
		Atlas:                atlas.NewClient(atlasServer.URL, 5*time.Second),
		DefaultCPUMillicores: 1000,
		DefaultMemoryMiB:     1024,
		DiskMiB:              10240,
		PollInterval:         5 * time.Millisecond,
		OperationTimeout:     2 * time.Second,
		Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	t.Cleanup(adapter.Close)
	return &harness{atlas: fake, adapter: adapter}
}

func (test *harness) call(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, test.adapter.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer good")
	request.Header.Set("X-Tenant-ID", "7")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func (test *harness) create(t *testing.T, name string) string {
	t.Helper()
	status, body := test.call(t, http.MethodPost, "/v1.54/containers/create?name="+name, `{"Image":"ubuntu-24.04","AttachStdout":false,"HostConfig":{"NanoCpus":2000000000,"Memory":2147483648,"RestartPolicy":{"Name":"no"},"NetworkMode":"default"}}`)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	var created struct{ Id string }
	json.Unmarshal([]byte(body), &created)
	return created.Id
}

func TestPingNeedsNoCredentialsButOtherCallsDo(t *testing.T) {
	test := newHarness(t)

	response, err := http.Head(test.adapter.URL + "/_ping")
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Api-Version") != APIVersion {
		t.Fatalf("ping: %v %v", response, err)
	}
	response, err = http.Get(test.adapter.URL + "/v1.54/version")
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("version without credentials: %v %v", response, err)
	}
	if status, _ := test.call(t, http.MethodGet, "/v1.99/version", ""); status != http.StatusBadRequest {
		t.Fatalf("unsupported API version: %d", status)
	}
}

func TestCreateMapsResourcesAndTags(t *testing.T) {
	test := newHarness(t)
	id := test.create(t, "web")

	request := test.atlas.created[0]
	if request["image_id"] != "img-ubuntu" || request["cpu_millicores"] != float64(2000) || request["memory_mib"] != float64(2048) || request["disk_mib"] != float64(10240) {
		t.Fatalf("Atlas create request: %v", request)
	}
	tags := test.atlas.machine(id).tags
	if tags[managedTag] != "true" || tags[nameTag] != "web" || tags[imageTag] != "ubuntu-24.04" {
		t.Fatalf("tags: %v", tags)
	}

	if status, body := test.call(t, http.MethodPost, "/v1.54/containers/create?name=web", `{"Image":"ubuntu-24.04"}`); status != http.StatusConflict {
		t.Fatalf("duplicate name: %d %s", status, body)
	}
}

func TestCreateRejectsBeforeCallingAtlas(t *testing.T) {
	test := newHarness(t)
	tests := []struct {
		name, body string
		status     int
	}{
		{"command", `{"Image":"ubuntu-24.04","Cmd":["echo","hi"]}`, http.StatusBadRequest},
		{"published port", `{"Image":"ubuntu-24.04","HostConfig":{"PortBindings":{"80/tcp":[{"HostPort":"8080"}]}}}`, http.StatusBadRequest},
		{"auto remove", `{"Image":"ubuntu-24.04","HostConfig":{"AutoRemove":true}}`, http.StatusBadRequest},
		{"fractional memory", `{"Image":"ubuntu-24.04","HostConfig":{"Memory":1000}}`, http.StatusBadRequest},
		{"too little CPU", `{"Image":"ubuntu-24.04","HostConfig":{"NanoCpus":50000000}}`, http.StatusBadRequest},
		{"unknown image", `{"Image":"nginx"}`, http.StatusNotFound},
		{"disabled image", `{"Image":"retired"}`, http.StatusNotFound},
	}
	for _, rejected := range tests {
		t.Run(rejected.name, func(t *testing.T) {
			if status, body := test.call(t, http.MethodPost, "/v1.54/containers/create", rejected.body); status != rejected.status {
				t.Fatalf("status %d: %s", status, body)
			}
		})
	}
	if len(test.atlas.created) != 0 {
		t.Fatalf("Atlas received %d creates", len(test.atlas.created))
	}
}

func TestDetachedRunLifecycle(t *testing.T) {
	test := newHarness(t)
	id := test.create(t, "web")

	// the cli waits for these headers before sending start.
	waitRequest, _ := http.NewRequest(http.MethodPost, test.adapter.URL+"/v1.54/containers/"+id+"/wait?condition=next-exit", nil)
	waitRequest.Header.Set("Authorization", "Bearer good")
	waitRequest.Header.Set("X-Tenant-ID", "7")
	waitResponse, err := http.DefaultClient.Do(waitRequest)
	if err != nil || waitResponse.StatusCode != http.StatusOK {
		t.Fatalf("wait headers: %v %v", waitResponse, err)
	}
	defer waitResponse.Body.Close()

	// creation may have finished booting; the cli accepts 304 from start.
	if status, body := test.call(t, http.MethodPost, "/v1.54/containers/web/start", ""); status != http.StatusNoContent && status != http.StatusNotModified {
		t.Fatalf("start: %d %s", status, body)
	}
	if status, _ := test.call(t, http.MethodPost, "/v1.54/containers/web/start", ""); status != http.StatusNotModified {
		t.Fatalf("start when running: %d", status)
	}
	if status, body := test.call(t, http.MethodGet, "/v1.54/containers/json", ""); status != http.StatusOK || !strings.Contains(body, `"/web"`) || strings.Contains(body, "vm-infra") {
		t.Fatalf("list: %d %s", status, body)
	}

	if status, body := test.call(t, http.MethodPost, "/v1.54/containers/web/stop", ""); status != http.StatusNoContent {
		t.Fatalf("stop: %d %s", status, body)
	}
	var result struct{ StatusCode int }
	if err := json.NewDecoder(waitResponse.Body).Decode(&result); err != nil || result.StatusCode != 0 {
		t.Fatalf("wait result: %+v %v", result, err)
	}

	if _, body := test.call(t, http.MethodGet, "/v1.54/containers/json", ""); strings.Contains(body, `"/web"`) {
		t.Fatalf("stopped container in default list: %s", body)
	}
	if _, body := test.call(t, http.MethodGet, "/v1.54/containers/json?all=1", ""); !strings.Contains(body, `"exited"`) {
		t.Fatalf("stopped container missing from all: %s", body)
	}
}

func TestInspect(t *testing.T) {
	test := newHarness(t)
	id := test.create(t, "web")
	test.call(t, http.MethodPost, "/v1.54/containers/web/start", "")

	status, body := test.call(t, http.MethodGet, "/v1.54/containers/"+id[:5]+"/json", "")
	if status != http.StatusOK {
		t.Fatalf("inspect by prefix: %d %s", status, body)
	}
	var inspected struct {
		Id, Name string
		State    struct{ Running bool }
		Config   struct{ Image string }
	}
	json.Unmarshal([]byte(body), &inspected)
	if inspected.Id != id || inspected.Name != "/web" || !inspected.State.Running || inspected.Config.Image != "ubuntu-24.04" {
		t.Fatalf("inspect: %s", body)
	}

	if status, _ := test.call(t, http.MethodGet, "/v1.54/containers/vm-infra/json", ""); status != http.StatusNotFound {
		t.Fatalf("unmanaged VM visible: %d", status)
	}
}

func TestRemove(t *testing.T) {
	test := newHarness(t)
	id := test.create(t, "web")
	test.call(t, http.MethodPost, "/v1.54/containers/web/start", "")

	if status, body := test.call(t, http.MethodDelete, "/v1.54/containers/web", ""); status != http.StatusConflict {
		t.Fatalf("remove running: %d %s", status, body)
	}
	if status, body := test.call(t, http.MethodDelete, "/v1.54/containers/web?force=1", ""); status != http.StatusNoContent {
		t.Fatalf("force remove: %d %s", status, body)
	}
	if test.atlas.machine(id) != nil {
		t.Fatal("VM still exists in Atlas")
	}
}

func TestUnsupportedOperations(t *testing.T) {
	test := newHarness(t)
	test.create(t, "web")
	for _, unsupported := range []struct{ method, path string }{
		{http.MethodPost, "/v1.54/containers/web/attach?stream=1&stdout=1"},
		{http.MethodGet, "/v1.54/containers/web/logs"},
		{http.MethodPost, "/v1.54/containers/web/exec"},
		{http.MethodPost, "/v1.54/images/create?fromImage=nginx"},
		{http.MethodGet, "/v1.54/networks"},
	} {
		if status, body := test.call(t, unsupported.method, unsupported.path, ""); status != http.StatusNotImplemented || !strings.Contains(body, "message") {
			t.Fatalf("%s %s: %d %s", unsupported.method, unsupported.path, status, body)
		}
	}
}

func TestImages(t *testing.T) {
	test := newHarness(t)
	status, body := test.call(t, http.MethodGet, "/v1.54/images/json", "")
	if status != http.StatusOK || !strings.Contains(body, "ubuntu-24.04") || strings.Contains(body, "retired") {
		t.Fatalf("images: %d %s", status, body)
	}
}

func TestAtlasRefusalIsPassedOn(t *testing.T) {
	test := newHarness(t)
	request, _ := http.NewRequest(http.MethodGet, test.adapter.URL+"/v1.54/containers/json", nil)
	request.Header.Set("Authorization", "Bearer expired")
	request.Header.Set("X-Tenant-ID", "7")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(data), "Invalid token") {
		t.Fatalf("expired token: %d %s", response.StatusCode, data)
	}
}

func TestAtlasFieldErrorsReachTheDockerUser(t *testing.T) {
	test := newHarness(t)
	test.atlas.rejectCreate = true
	status, body := test.call(t, http.MethodPost, "/v1.54/containers/create?name=web", `{"Image":"ubuntu-24.04"}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "tags: Extra inputs are not permitted") {
		t.Fatalf("status %d: %s", status, body)
	}
}
