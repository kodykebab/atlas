package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// each vm read advances its state; use explicit state responses to test stale observations.
type fakeAtlas struct {
	mutex        sync.Mutex
	next         int
	machines     map[string]*fakeMachine
	images       []map[string]any
	created      []map[string]any
	rejectCreate bool
}

type fakeMachine struct {
	id, imageID string
	current     string
	desired     string
	deleting    bool
	cpu, memory int
	tags        map[string]string
}

func newFakeAtlas() *fakeAtlas {
	return &fakeAtlas{
		machines: map[string]*fakeMachine{
			"vm-infra": {id: "vm-infra", imageID: "img-ubuntu", current: "running", desired: "running", cpu: 1000, memory: 1024, tags: map[string]string{"role": "cargo"}},
		},
		images: []map[string]any{
			{"id": "img-ubuntu", "title": "ubuntu-24.04", "architecture": "amd64", "status": "available", "enabled": true, "created_at": 1790000000, "rootfs_size_mib": 2048},
			{"id": "img-disabled", "title": "retired", "architecture": "amd64", "status": "available", "enabled": false, "created_at": 1790000000, "rootfs_size_mib": 2048},
		},
	}
}

func (fake *fakeAtlas) machine(id string) *fakeMachine {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.machines[id]
}

func (fake *fakeAtlas) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer good" || request.Header.Get("X-Tenant-ID") != "7" {
		reply(writer, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "Invalid token.", "fields": []any{}}})
		return
	}

	fake.mutex.Lock()
	defer fake.mutex.Unlock()

	path := strings.TrimPrefix(request.URL.Path, "/api/atlas")
	segments := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case request.Method == http.MethodGet && path == "/images":
		reply(writer, http.StatusOK, map[string]any{"items": fake.images, "has_more": false, "limit": 100, "offset": 0})

	case request.Method == http.MethodGet && path == "/virtual-machines":
		key, value, _ := strings.Cut(request.URL.Query().Get("tag"), ":")
		items := []map[string]any{}
		for _, machine := range fake.machines {
			if machine.tags[key] != value {
				continue
			}
			items = append(items, map[string]any{
				"id": machine.id, "image_id": machine.imageID, "cpu_millicores": machine.cpu, "memory_mib": machine.memory,
				"disk_mib": 10240, "created_at": 1790000000, "last_known_state": machine.current, "tags": machine.tags,
			})
		}
		reply(writer, http.StatusOK, map[string]any{"items": items, "has_more": false, "limit": 100, "offset": 0})

	case request.Method == http.MethodPost && path == "/virtual-machines":
		if fake.rejectCreate {
			reply(writer, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "invalid_request", "message": "The request is not valid.", "fields": []map[string]string{{"name": "tags", "message": "Extra inputs are not permitted"}}}})
			return
		}
		var body map[string]any
		json.NewDecoder(request.Body).Decode(&body)
		fake.created = append(fake.created, body)
		fake.next++
		id := fmt.Sprintf("vm-%07d", fake.next)
		tags := map[string]string{}
		for key, value := range body["tags"].(map[string]any) {
			tags[key] = value.(string)
		}
		fake.machines[id] = &fakeMachine{
			id: id, imageID: body["image_id"].(string), current: "pending", desired: "running",
			cpu: int(body["cpu_millicores"].(float64)), memory: int(body["memory_mib"].(float64)), tags: tags,
		}
		reply(writer, http.StatusAccepted, map[string]any{"id": id, "image_id": body["image_id"], "tags": tags, "created_at": 1790000000})

	case len(segments) == 2 && segments[0] == "virtual-machines":
		machine, found := fake.machines[segments[1]]
		if !found {
			reply(writer, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "Virtual machine not found.", "fields": []any{}}})
			return
		}
		if request.Method == http.MethodDelete {
			machine.deleting = true
			reply(writer, http.StatusAccepted, map[string]any{"id": machine.id})
			return
		}
		if machine.deleting {
			if machine.current == "terminating" {
				delete(fake.machines, machine.id)
				reply(writer, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "Virtual machine not found.", "fields": []any{}}})
				return
			}
			machine.current = "terminating"
		} else {
			machine.advance()
		}
		desired := machine.desired
		reply(writer, http.StatusOK, map[string]any{
			"id": machine.id, "image_id": machine.imageID, "architecture": "amd64", "created_at": 1790000000,
			"current_state": machine.current, "desired_state": desired, "error": nil, "tags": machine.tags,
			"compute": map[string]any{"cpu_millicores": machine.cpu, "memory_mib": machine.memory},
			"disk":    map[string]any{"size_mib": 10240}, "guest": map[string]any{"hostname": nil},
			"network": map[string]any{"mesh_ipv6": "fdaa::1", "public_ipv4": nil},
		})

	case len(segments) == 4 && segments[0] == "virtual-machines" && segments[2] == "actions":
		machine, found := fake.machines[segments[1]]
		if !found {
			reply(writer, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "Virtual machine not found.", "fields": []any{}}})
			return
		}
		switch segments[3] {
		case "start", "resume":
			machine.desired = "running"
		case "stop":
			machine.desired = "stopped"
		case "pause":
			machine.desired = "paused"
		case "restart":
			machine.current = "stopped"
			machine.desired = "running"
		}
		reply(writer, http.StatusAccepted, map[string]any{"id": machine.id})

	default:
		reply(writer, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "No route.", "fields": []any{}}})
	}
}

func (machine *fakeMachine) advance() {
	switch {
	case machine.current == machine.desired:
	case machine.current == "pending":
		machine.current = "created"
	default:
		machine.current = machine.desired
	}
}

func reply(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	json.NewEncoder(writer).Encode(value)
}
