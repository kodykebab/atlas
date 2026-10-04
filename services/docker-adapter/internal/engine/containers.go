package engine

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/frappe/atlas/services/docker-adapter/internal/atlas"
)

const (
	managedTag     = "docker.managed"
	managedTagText = "docker.managed:true"
	nameTag        = "docker.name"
	imageTag       = "docker.image"
)

func dockerState(state string) string {
	switch state {
	case "running":
		return "running"
	case "paused":
		return "paused"
	case "stopped":
		return "exited"
	case "failed":
		return "dead"
	case "terminating", "destroyed":
		return "removing"
	default:
		// docker has no unknown state.
		return "created"
	}
}

func (server *Server) create(writer http.ResponseWriter, request *http.Request, caller atlas.Caller) {
	body, err := decodeCreate(http.MaxBytesReader(writer, request.Body, 1<<20))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid create request: "+err.Error())
		return
	}
	cpu, err := cpuMillicores(body.HostConfig.NanoCpus, server.config.DefaultCPUMillicores)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	memory, err := memoryMiB(body.HostConfig.Memory, server.config.DefaultMemoryMiB)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}

	name := strings.TrimPrefix(request.URL.Query().Get("name"), "/")
	if name == "" {
		name = generatedName()
	}
	if !validName(name) {
		writeError(writer, http.StatusBadRequest, fmt.Sprintf("invalid container name %q: use letters, digits, '_', '.' and '-'", name))
		return
	}

	ctx := request.Context()
	containers, err := server.config.Atlas.VirtualMachines(ctx, caller, managedTagText)
	if err != nil {
		writeAtlasError(writer, err)
		return
	}
	for _, existing := range containers {
		if existing.Tags[nameTag] == name {
			writeError(writer, http.StatusConflict, fmt.Sprintf("Conflict. The container name \"/%s\" is already in use by container %q. Remove or rename it first.", name, existing.ID))
			return
		}
	}

	image, err := server.resolveImage(ctx, caller, body.Image)
	if err != nil {
		if errors.Is(err, errNoSuchImage) {
			writeError(writer, http.StatusNotFound, "No such image: "+body.Image)
			return
		}
		writeAtlasError(writer, err)
		return
	}

	created, err := server.config.Atlas.CreateVirtualMachine(ctx, caller, atlas.CreateRequest{
		ImageID:       image.ID,
		CPUMillicores: cpu,
		MemoryMiB:     memory,
		DiskMiB:       server.config.DiskMiB,
		Hostname:      body.Hostname,
		Tags:          map[string]string{managedTag: "true", nameTag: name, imageTag: body.Image},
	})
	if err != nil {
		writeAtlasError(writer, err)
		return
	}
	server.config.Logger.Info("container created", "id", created.ID, "name", name, "image", image.ID, "tenant", caller.TenantID)
	writeJSON(writer, http.StatusCreated, map[string]any{
		"Id":       created.ID,
		"Warnings": []string{"Atlas boots a VM as soon as it is created; docker start waits until it is running."},
	})
}

func (server *Server) list(writer http.ResponseWriter, request *http.Request, caller atlas.Caller) {
	query := request.URL.Query()
	all := query.Get("all") == "1" || query.Get("all") == "true"

	containers, err := server.config.Atlas.VirtualMachines(request.Context(), caller, managedTagText)
	if err != nil {
		writeAtlasError(writer, err)
		return
	}
	slices.SortFunc(containers, func(first, second atlas.VirtualMachine) int {
		return cmp.Compare(second.CreatedAt, first.CreatedAt)
	})

	items := []map[string]any{}
	for _, container := range containers {
		state := dockerState(container.LastKnownState)
		if container.Tags[managedTag] != "true" {
			continue
		}
		if !all && state != "running" && state != "paused" {
			continue
		}
		items = append(items, map[string]any{
			"Id":              container.ID,
			"Names":           []string{"/" + container.Tags[nameTag]},
			"Image":           container.Tags[imageTag],
			"ImageID":         container.ImageID,
			"Command":         "",
			"Created":         container.CreatedAt,
			"State":           state,
			"Status":          statusText(state),
			"Ports":           []any{},
			"Labels":          map[string]string{},
			"Mounts":          []any{},
			"HostConfig":      map[string]string{"NetworkMode": "default"},
			"NetworkSettings": map[string]any{"Networks": map[string]any{}},
		})
	}
	writeJSON(writer, http.StatusOK, items)
}

func (server *Server) inspect(writer http.ResponseWriter, request *http.Request, caller atlas.Caller, reference string) {
	container, ok := server.resolve(writer, request, caller, reference)
	if !ok {
		return
	}
	detail, err := server.config.Atlas.VirtualMachine(request.Context(), caller, container.ID)
	if err != nil {
		writeAtlasError(writer, err)
		return
	}

	state := dockerState(detail.CurrentState)
	errorText := ""
	if detail.Error != nil {
		errorText = *detail.Error
	}
	hostname := ""
	if detail.Guest.Hostname != nil {
		hostname = *detail.Guest.Hostname
	}
	addresses := map[string]any{}
	if detail.Network.MeshIPv6 != nil {
		addresses["GlobalIPv6Address"] = *detail.Network.MeshIPv6
	}
	if detail.Network.PublicIPv4 != nil {
		addresses["IPAddress"] = *detail.Network.PublicIPv4
	}

	// process ids and timestamps are unavailable; exit codes describe vm state only.
	writeJSON(writer, http.StatusOK, map[string]any{
		"Id":       detail.ID,
		"Created":  time.Unix(detail.CreatedAt, 0).UTC().Format(time.RFC3339Nano),
		"Path":     "",
		"Args":     []string{},
		"Name":     "/" + detail.Tags[nameTag],
		"Image":    detail.ImageID,
		"Platform": "linux",
		"State": map[string]any{
			"Status":     state,
			"Running":    state == "running" || state == "paused",
			"Paused":     state == "paused",
			"Restarting": false,
			"OOMKilled":  false,
			"Dead":       state == "dead",
			"Pid":        0,
			"ExitCode":   exitCode(detail.CurrentState),
			"Error":      errorText,
			"StartedAt":  "0001-01-01T00:00:00Z",
			"FinishedAt": "0001-01-01T00:00:00Z",
		},
		"Config": map[string]any{
			"Hostname": hostname,
			"Image":    detail.Tags[imageTag],
			"Labels":   map[string]string{},
		},
		"HostConfig": map[string]any{
			"NanoCpus":    int64(detail.Compute.CPUMillicores) * 1_000_000,
			"Memory":      int64(detail.Compute.MemoryMiB) << 20,
			"NetworkMode": "default",
		},
		"NetworkSettings": addresses,
		"Mounts":          []any{},
		"RestartCount":    0,
	})
}

func (server *Server) start(writer http.ResponseWriter, request *http.Request, caller atlas.Caller, reference string) {
	container, ok := server.resolve(writer, request, caller, reference)
	if !ok {
		return
	}
	ctx := request.Context()
	detail, err := server.config.Atlas.VirtualMachine(ctx, caller, container.ID)
	if err != nil {
		writeAtlasError(writer, err)
		return
	}
	if detail.CurrentState == "running" && detail.DesiredState != nil && *detail.DesiredState == "running" {
		writer.WriteHeader(http.StatusNotModified)
		return
	}

	// atlas already boots new vms during creation.
	if detail.DesiredState == nil || *detail.DesiredState != "running" {
		if err := server.config.Atlas.Act(ctx, caller, container.ID, "start"); err != nil {
			writeAtlasError(writer, err)
			return
		}
	}
	server.respondWhenState(writer, request, caller, container.ID, "running")
}

func (server *Server) stop(writer http.ResponseWriter, request *http.Request, caller atlas.Caller, reference string) {
	container, ok := server.resolve(writer, request, caller, reference)
	if !ok {
		return
	}
	ctx := request.Context()
	detail, err := server.config.Atlas.VirtualMachine(ctx, caller, container.ID)
	if err != nil {
		writeAtlasError(writer, err)
		return
	}
	if detail.CurrentState == "stopped" && detail.DesiredState != nil && *detail.DesiredState == "stopped" {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	if err := server.config.Atlas.Act(ctx, caller, container.ID, "stop"); err != nil {
		writeAtlasError(writer, err)
		return
	}
	server.respondWhenState(writer, request, caller, container.ID, "stopped")
}

func (server *Server) transition(writer http.ResponseWriter, request *http.Request, caller atlas.Caller, reference, action, state string) {
	container, ok := server.resolve(writer, request, caller, reference)
	if !ok {
		return
	}
	if err := server.config.Atlas.Act(request.Context(), caller, container.ID, action); err != nil {
		writeAtlasError(writer, err)
		return
	}
	server.respondWhenState(writer, request, caller, container.ID, state)
}

func (server *Server) remove(writer http.ResponseWriter, request *http.Request, caller atlas.Caller, reference string) {
	query := request.URL.Query()
	force := query.Get("force") == "1" || query.Get("force") == "true"

	container, ok := server.resolve(writer, request, caller, reference)
	if !ok {
		return
	}
	ctx := request.Context()
	detail, err := server.config.Atlas.VirtualMachine(ctx, caller, container.ID)
	if err != nil {
		writeAtlasError(writer, err)
		return
	}
	if detail.CurrentState != "stopped" && detail.CurrentState != "failed" && !force {
		writeError(writer, http.StatusConflict, fmt.Sprintf("cannot remove container %q: it is %s. Stop it first, or use --force", "/"+container.Tags[nameTag], dockerState(detail.CurrentState)))
		return
	}
	if err := server.config.Atlas.DeleteVirtualMachine(ctx, caller, container.ID); err != nil {
		writeAtlasError(writer, err)
		return
	}

	err = server.poll(ctx, func(ctx context.Context) (bool, error) {
		_, err := server.config.Atlas.VirtualMachine(ctx, caller, container.ID)
		if atlas.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		server.writeWaitError(writer, err, container.ID, "removed")
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) resolve(writer http.ResponseWriter, request *http.Request, caller atlas.Caller, reference string) (atlas.VirtualMachine, bool) {
	containers, err := server.config.Atlas.VirtualMachines(request.Context(), caller, managedTagText)
	if err != nil {
		writeAtlasError(writer, err)
		return atlas.VirtualMachine{}, false
	}

	reference = strings.TrimPrefix(reference, "/")
	// full ids win over names; atlas does not enforce name uniqueness.
	for _, container := range containers {
		if container.Tags[managedTag] == "true" && container.ID == reference {
			return container, true
		}
	}
	var named, prefixed []atlas.VirtualMachine
	for _, container := range containers {
		if container.Tags[managedTag] != "true" {
			continue
		}
		if container.Tags[nameTag] == reference {
			named = append(named, container)
		}
		if reference != "" && strings.HasPrefix(container.ID, reference) {
			prefixed = append(prefixed, container)
		}
	}
	if len(named) == 1 {
		return named[0], true
	}
	if len(named) > 1 {
		writeError(writer, http.StatusConflict, fmt.Sprintf("multiple containers have name %q; use the full ID", reference))
		return atlas.VirtualMachine{}, false
	}
	switch len(prefixed) {
	case 1:
		return prefixed[0], true
	case 0:
		writeError(writer, http.StatusNotFound, "No such container: "+reference)
	default:
		writeError(writer, http.StatusBadRequest, fmt.Sprintf("multiple containers match %q; use a longer ID or the name", reference))
	}
	return atlas.VirtualMachine{}, false
}

func statusText(state string) string {
	switch state {
	case "running":
		return "Up"
	case "paused":
		return "Up (Paused)"
	case "exited":
		return "Exited"
	case "dead":
		return "Dead"
	case "removing":
		return "Removal In Progress"
	default:
		return "Created"
	}
}

func generatedName() string {
	random := make([]byte, 4)
	rand.Read(random)
	return "atlas-" + hex.EncodeToString(random)
}

func exitCode(state string) int {
	if state == "failed" {
		return 1
	}
	return 0
}
