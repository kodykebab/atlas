package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frappe/atlas/services/docker-adapter/internal/atlas"
)

func newEngineServer(t *testing.T, upstream http.HandlerFunc) *Server {
	t.Helper()
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)
	return NewServer(Config{Atlas: atlas.NewClient(backend.URL, time.Second), DefaultCPUMillicores: 1000, DefaultMemoryMiB: 1024, DiskMiB: 10240, PollInterval: time.Millisecond, OperationTimeout: 30 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func serveRequest(server *Server, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx, cancel := context.WithTimeout(request.Context(), 300*time.Millisecond)
	defer cancel()
	request = request.WithContext(ctx)
	request.Header.Set("Authorization", "Bearer good")
	request.Header.Set("X-Tenant-ID", "7")
	writer := httptest.NewRecorder()
	server.ServeHTTP(writer, request)
	return writer
}

func TestPollDeadlineCancelsUpstream(t *testing.T) {
	server := newEngineServer(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	started := time.Now()
	err := server.poll(context.Background(), func(ctx context.Context) (bool, error) {
		_, err := server.config.Atlas.VirtualMachine(ctx, atlas.Caller{}, "vm-1")
		return false, err
	})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("HTTP call outlived deadline: %v after %s", err, time.Since(started))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = server.poll(ctx, func(context.Context) (bool, error) { t.Fatal("called upstream after cancellation"); return true, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestResolveNeverSelectsAmbiguousName(t *testing.T) {
	server := newEngineServer(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"has_more": false, "items": []any{
			map[string]any{"id": "vm-2", "tags": map[string]string{managedTag: "true", nameTag: "vm-1"}},
			map[string]any{"id": "vm-1", "tags": map[string]string{managedTag: "true", nameTag: "duplicate"}},
			map[string]any{"id": "vm-3", "tags": map[string]string{managedTag: "true", nameTag: "duplicate"}},
			map[string]any{"id": "vm-infra", "tags": map[string]string{nameTag: "infra"}},
		}})
	})
	for _, test := range []struct {
		reference, id string
		status        int
	}{{"vm-1", "vm-1", 200}, {"duplicate", "", 409}, {"infra", "", 404}, {"vm-", "", 400}, {"", "", 404}} {
		t.Run(test.reference, func(t *testing.T) {
			w := httptest.NewRecorder()
			vm, ok := server.resolve(w, httptest.NewRequest("GET", "/", nil), atlas.Caller{}, test.reference)
			if w.Code != test.status || vm.ID != test.id || ok != (test.id != "") {
				t.Fatalf("got %d %+v %v", w.Code, vm, ok)
			}
		})
	}
}

func TestPausedVMIsRunningForDocker(t *testing.T) {
	test := newHarness(t)
	test.create(t, "paused")
	if status, body := test.call(t, "POST", "/containers/paused/pause", ""); status != 204 {
		t.Fatalf("pause: %d %s", status, body)
	}
	if status, body := test.call(t, "GET", "/containers/json", ""); status != 200 || !strings.Contains(body, `"paused"`) {
		t.Fatalf("list: %d %s", status, body)
	}
	if _, body := test.call(t, "GET", "/containers/paused/json", ""); !strings.Contains(body, `"Running":true`) || !strings.Contains(body, `"Paused":true`) {
		t.Fatalf("inspect: %s", body)
	}
}

func TestWaitDefaultReturnsForStoppedVM(t *testing.T) {
	server := newEngineServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/atlas/virtual-machines" {
			reply(w, 200, map[string]any{"has_more": false, "items": []any{map[string]any{"id": "vm-1", "tags": map[string]string{managedTag: "true", nameTag: "web"}}}})
			return
		}
		reply(w, 200, map[string]any{"id": "vm-1", "current_state": "stopped"})
	})
	response := serveRequest(server, "POST", "/containers/web/wait", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"StatusCode":0`) {
		t.Fatalf("wait: %d %s", response.Code, response.Body.String())
	}
}

func TestCreateRejectsTrailingJSONAndNegativeMemory(t *testing.T) {
	server := newEngineServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid create reached Atlas")
		w.WriteHeader(500)
	})
	for _, body := range []string{`{"Image":"ubuntu"} {}`, `{"Image":"ubuntu"} junk`, `{"Image":"ubuntu","HostConfig":{"Memory":-1048576}}`} {
		if response := serveRequest(server, "POST", "/containers/create", body); response.Code != 400 {
			t.Fatalf("got %d %s", response.Code, response.Body.String())
		}
	}
}

func TestDeleteDoesNotTreatForbiddenAsGone(t *testing.T) {
	deleted := false
	server := newEngineServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/atlas/virtual-machines":
			reply(w, 200, map[string]any{"has_more": false, "items": []any{map[string]any{"id": "vm-1", "tags": map[string]string{managedTag: "true", nameTag: "web"}}}})
		case r.Method == "DELETE":
			deleted = true
			w.WriteHeader(202)
		case deleted:
			reply(w, 403, map[string]any{"error": map[string]string{"message": "Permission denied"}})
		default:
			reply(w, 200, map[string]any{"id": "vm-1", "current_state": "stopped"})
		}
	})
	response := serveRequest(server, "DELETE", "/containers/web", "")
	if response.Code != 403 {
		t.Fatalf("got %d %s", response.Code, response.Body.String())
	}
}

func TestPowerActionDoesNotIgnoreContraryOrMissingIntent(t *testing.T) {
	for _, test := range []struct {
		name, action, current string
		desired               *string
	}{
		{name: "start with missing intent", action: "start", current: "stopped"},
		{name: "start while stopping", action: "start", current: "running", desired: pointer("stopped")},
		{name: "stop while starting", action: "stop", current: "stopped", desired: pointer("running")},
	} {
		t.Run(test.name, func(t *testing.T) {
			acted := false
			server := newEngineServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/atlas/virtual-machines" {
					reply(w, 200, map[string]any{"has_more": false, "items": []any{map[string]any{"id": "vm-1", "tags": map[string]string{managedTag: "true", nameTag: "web"}}}})
				} else if r.Method == "POST" {
					if !strings.HasSuffix(r.URL.Path, "/actions/"+test.action) {
						t.Errorf("unexpected action %s", r.URL.Path)
					}
					acted = true
					w.WriteHeader(202)
				} else {
					current := test.current
					desired := test.desired
					if acted {
						if test.action == "start" {
							current = "running"
						} else {
							current = "stopped"
						}
						desired = pointer(current)
					}
					reply(w, 200, map[string]any{"id": "vm-1", "current_state": current, "desired_state": desired})
				}
			})
			response := serveRequest(server, "POST", "/containers/web/"+test.action, "")
			if response.Code != 204 || !acted {
				t.Fatalf("action skipped: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func pointer(value string) *string { return &value }

func TestOperationDeadlineIncludesLookup(t *testing.T) {
	server := newEngineServer(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	started := time.Now()
	response := serveRequest(server, "POST", "/containers/create", `{"Image":"ubuntu"}`)
	if response.Code != http.StatusGatewayTimeout || time.Since(started) > 200*time.Millisecond {
		t.Fatalf("lookup was not bounded by operation timeout: %d %s", response.Code, response.Body.String())
	}
}

func TestWaitSetupDeadlineIncludesLookup(t *testing.T) {
	server := newEngineServer(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	response := serveRequest(server, "POST", "/containers/web/wait", "")
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("wait lookup was not bounded: %d %s", response.Code, response.Body.String())
	}
}

func TestNextExitReportsFailureBeforeFirstRunningObservation(t *testing.T) {
	server := newEngineServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/atlas/virtual-machines" {
			reply(w, 200, map[string]any{"has_more": false, "items": []any{map[string]any{"id": "vm-1", "tags": map[string]string{managedTag: "true", nameTag: "web"}}}})
			return
		}
		reply(w, 200, map[string]any{"id": "vm-1", "current_state": "failed", "error": "boot failed"})
	})
	response := serveRequest(server, "POST", "/containers/web/wait?condition=next-exit", "")
	if !strings.Contains(response.Body.String(), `"StatusCode":1`) || !strings.Contains(response.Body.String(), "boot failed") {
		t.Fatalf("failed boot did not finish wait: %s", response.Body.String())
	}
}

func TestTransitionDoesNotCompleteWithContraryIntent(t *testing.T) {
	server := newEngineServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/atlas/virtual-machines" {
			reply(w, 200, map[string]any{"has_more": false, "items": []any{map[string]any{"id": "vm-1", "tags": map[string]string{managedTag: "true", nameTag: "web"}}}})
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		reply(w, 200, map[string]any{"id": "vm-1", "current_state": "running", "desired_state": "stopped"})
	})
	response := serveRequest(server, "POST", "/containers/web/start", "")
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("contrary intent reported as completed: %d %s", response.Code, response.Body.String())
	}
}
