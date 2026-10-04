// package engine translates docker requests into atlas vm operations.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/frappe/atlas/services/docker-adapter/internal/atlas"
)

const (
	APIVersion          = "1.54"
	minimumAPIVersion   = "1.44"
	maximumMinorVersion = 54
	minimumMinorVersion = 44
)

var versionPrefix = regexp.MustCompile(`^/v([0-9]+)\.([0-9]+)(/.*)$`)

type Config struct {
	Atlas                *atlas.Client
	DefaultCPUMillicores int
	DefaultMemoryMiB     int
	DiskMiB              int
	PollInterval         time.Duration
	OperationTimeout     time.Duration
	Logger               *slog.Logger
}

type Server struct {
	config Config
}

func NewServer(config Config) *Server {
	return &Server{config: config}
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	if match := versionPrefix.FindStringSubmatch(path); match != nil {
		if !supportedVersion(match[1], match[2]) {
			writeError(writer, http.StatusBadRequest, fmt.Sprintf("API version %s.%s is not supported; this adapter supports %s to %s", match[1], match[2], minimumAPIVersion, APIVersion))
			return
		}
		path = match[3]
	}

	if path == "/_ping" && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		setVersionHeaders(writer)
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		if request.Method == http.MethodGet {
			writer.Write([]byte("OK"))
		}
		return
	}

	setVersionHeaders(writer)
	caller, ok := callerOf(request)
	if !ok {
		writeError(writer, http.StatusUnauthorized, "set Authorization and X-Tenant-ID for Atlas in the HttpHeaders of your Docker config.json")
		return
	}

	segments := strings.Split(strings.Trim(path, "/"), "/")
	if err := admitQuery(request, path); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	// wait streams last until completion or cancellation, outside the operation deadline.
	if !(request.Method == http.MethodPost && len(segments) == 3 && segments[0] == "containers" && segments[2] == "wait") {
		ctx, cancel := context.WithTimeout(request.Context(), server.config.OperationTimeout)
		defer cancel()
		request = request.WithContext(ctx)
	}

	switch {
	case request.Method == http.MethodGet && path == "/version":
		server.version(writer)
	case request.Method == http.MethodGet && path == "/info":
		server.info(writer)
	case request.Method == http.MethodGet && path == "/images/json":
		server.listImages(writer, request, caller)
	case request.Method == http.MethodPost && path == "/images/create":
		writeError(writer, http.StatusNotImplemented, "Atlas images cannot be pulled. Use an Atlas image ID or title from docker images, with --pull=never")
	case request.Method == http.MethodPost && path == "/containers/create":
		server.create(writer, request, caller)
	case request.Method == http.MethodGet && path == "/containers/json":
		server.list(writer, request, caller)
	case len(segments) == 3 && segments[0] == "containers" && request.Method == http.MethodGet && segments[2] == "json":
		server.inspect(writer, request, caller, segments[1])
	case len(segments) == 3 && segments[0] == "containers" && request.Method == http.MethodPost:
		server.containerAction(writer, request, caller, segments[1], segments[2])
	case len(segments) == 2 && segments[0] == "containers" && request.Method == http.MethodDelete:
		server.remove(writer, request, caller, segments[1])
	default:
		writeError(writer, http.StatusNotImplemented, fmt.Sprintf("%s %s is not supported: the Atlas Docker adapter runs VMs and supports only create, start, stop, pause, unpause, wait, ps, inspect, rm and images", request.Method, path))
	}
}

func (server *Server) containerAction(writer http.ResponseWriter, request *http.Request, caller atlas.Caller, reference, action string) {
	switch action {
	case "start":
		server.start(writer, request, caller, reference)
	case "stop":
		server.stop(writer, request, caller, reference)
	case "restart":
		writeError(writer, http.StatusNotImplemented, "restart is not supported: Atlas cannot confirm restart completion; use stop followed by start")
	case "pause":
		server.transition(writer, request, caller, reference, "pause", "paused")
	case "unpause":
		server.transition(writer, request, caller, reference, "resume", "running")
	case "wait":
		server.wait(writer, request, caller, reference)
	default:
		writeError(writer, http.StatusNotImplemented, fmt.Sprintf("docker %s is not supported: an Atlas VM has no container process, console stream or exec", action))
	}
}

func (server *Server) version(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"Platform":      map[string]string{"Name": "Atlas Docker adapter"},
		"Version":       "atlas-adapter",
		"ApiVersion":    APIVersion,
		"MinAPIVersion": minimumAPIVersion,
		"Os":            "linux",
		"Arch":          runtime.GOARCH,
		"Components":    []any{},
	})
}

func (server *Server) info(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"ID":              "atlas",
		"Name":            "atlas",
		"ServerVersion":   "atlas-adapter",
		"OperatingSystem": "Atlas (containers are VMs)",
		"OSType":          "linux",
		"Architecture":    runtime.GOARCH,
		"Driver":          "atlas",
	})
}

func callerOf(request *http.Request) (atlas.Caller, bool) {
	caller := atlas.Caller{
		Authorization: request.Header.Get("Authorization"),
		TenantID:      request.Header.Get("X-Tenant-ID"),
	}
	return caller, caller.Authorization != "" && caller.TenantID != ""
}

func supportedVersion(major, minor string) bool {
	number, err := strconv.Atoi(minor)
	return major == "1" && err == nil && number >= minimumMinorVersion && number <= maximumMinorVersion
}

func setVersionHeaders(writer http.ResponseWriter) {
	writer.Header().Set("Api-Version", APIVersion)
	writer.Header().Set("OSType", "linux")
	writer.Header().Set("Docker-Experimental", "false")
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"message": message})
}

func writeAtlasError(writer http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(writer, http.StatusGatewayTimeout, "Atlas request timed out; check the VM state before retrying a mutation")
		return
	}

	var atlasError *atlas.Error
	if errors.As(err, &atlasError) {
		status := atlasError.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		writeError(writer, status, "Atlas: "+atlasError.Detail())
		return
	}
	writeError(writer, http.StatusBadGateway, "Atlas request failed: "+err.Error())
}

func admitQuery(request *http.Request, path string) error {
	if path == "/images/create" {
		return nil
	}

	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return fmt.Errorf("invalid query: %w", err)
	}
	for key, values := range query {
		if len(values) != 1 {
			return fmt.Errorf("duplicate query option %s", key)
		}
		value := values[0]
		allowed := false
		switch {
		case path == "/containers/create":
			allowed = key == "name"
		case path == "/containers/json":
			allowed = (key == "all" && (value == "1" || value == "0" || value == "true" || value == "false")) || (key == "filters" && value == "{}")
		case path == "/images/json":
			allowed = (key == "manifests" && (value == "0" || value == "false" || value == "1" || value == "true")) || (key == "all" && (value == "0" || value == "false")) || (key == "filters" && value == "{}")
		case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/wait"):
			allowed = key == "condition"
		case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
			allowed = key == "size" && (value == "0" || value == "false")
		case strings.HasPrefix(path, "/containers/") && request.Method == http.MethodDelete:
			allowed = (key == "force" && (value == "1" || value == "0" || value == "true" || value == "false")) || ((key == "v" || key == "link") && (value == "0" || value == "false"))
		}
		if !allowed {
			// unsupported routes keep their 501 response, including attach and logs.
			segments := strings.Split(strings.Trim(path, "/"), "/")
			if len(segments) == 3 && segments[0] == "containers" && !strings.Contains(" json start stop restart pause unpause wait ", " "+segments[2]+" ") {
				return nil
			}
			return fmt.Errorf("unsupported query option %s", key)
		}
	}
	return nil
}
