package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/frappe/atlas/services/docker-adapter/internal/atlas"
)

// the cli waits for response headers before sending start, so flush them before polling.
func (server *Server) wait(writer http.ResponseWriter, request *http.Request, caller atlas.Caller, reference string) {
	condition := request.URL.Query().Get("condition")
	if condition == "" {
		condition = "not-running"
	}
	if condition != "next-exit" && condition != "not-running" {
		writeError(writer, http.StatusBadRequest, fmt.Sprintf("wait condition %q is not supported", condition))
		return
	}
	lookup, cancel := context.WithTimeout(request.Context(), server.config.OperationTimeout)
	container, ok := server.resolve(writer, request.WithContext(lookup), caller, reference)
	cancel()
	if !ok {
		return
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}

	ctx := request.Context()
	// next-exit must not finish on a vm that was already stopped.
	seenRunning := condition == "not-running"
	var final atlas.VirtualMachineDetail
	err := server.pollUntil(ctx, 0, func(ctx context.Context) (bool, error) {
		detail, err := server.config.Atlas.VirtualMachine(ctx, caller, container.ID)
		if atlas.IsNotFound(err) {
			final = atlas.VirtualMachineDetail{CurrentState: "destroyed"}
			return true, nil
		}
		if err != nil {
			return false, err
		}
		final = detail
		switch detail.CurrentState {
		case "failed", "destroyed":
			return true, nil
		case "stopped":
			return seenRunning, nil
		default:
			seenRunning = true
			return false, nil
		}
	})
	if err != nil {
		// headers are already sent, so the body must carry the error.
		json.NewEncoder(writer).Encode(map[string]any{"StatusCode": -1, "Error": map[string]string{"Message": err.Error()}})
		return
	}

	result := map[string]any{"StatusCode": 0}
	if final.CurrentState == "failed" {
		message := "the Atlas VM failed"
		if final.Error != nil {
			message = *final.Error
		}
		result = map[string]any{"StatusCode": 1, "Error": map[string]string{"Message": message}}
	}
	json.NewEncoder(writer).Encode(result)
}

func (server *Server) respondWhenState(writer http.ResponseWriter, request *http.Request, caller atlas.Caller, id, state string) {
	ctx := request.Context()
	err := server.poll(ctx, func(ctx context.Context) (bool, error) {
		detail, err := server.config.Atlas.VirtualMachine(ctx, caller, id)
		if err != nil {
			return false, err
		}
		if detail.CurrentState == state && detail.DesiredState != nil && *detail.DesiredState == state {
			return true, nil
		}
		if detail.CurrentState == "failed" {
			message := "the Atlas VM failed"
			if detail.Error != nil {
				message = *detail.Error
			}
			return false, fmt.Errorf("%s", message)
		}
		return false, nil
	})
	if err != nil {
		server.writeWaitError(writer, err, id, state)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) writeWaitError(writer http.ResponseWriter, err error, id, state string) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(writer, http.StatusGatewayTimeout, fmt.Sprintf("Atlas accepted the request, but VM %s did not become %s within %s. Run docker inspect to see its state.", id, state, server.config.OperationTimeout))
		return
	}
	var atlasError *atlas.Error
	if errors.As(err, &atlasError) {
		writeAtlasError(writer, err)
		return
	}
	writeError(writer, http.StatusInternalServerError, fmt.Sprintf("VM %s did not become %s: %v", id, state, err))
}

func (server *Server) poll(ctx context.Context, check func(context.Context) (bool, error)) error {
	return server.pollUntil(ctx, server.config.OperationTimeout, check)
}

func (server *Server) pollUntil(ctx context.Context, timeout time.Duration, check func(context.Context) (bool, error)) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(server.config.PollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := check(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
