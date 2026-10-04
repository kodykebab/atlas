// package atlas calls the tenant api with the caller's credentials.
package atlas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const pageLimit = 100

type Caller struct {
	Authorization string
	TenantID      string
}

type VirtualMachine struct {
	ID             string            `json:"id"`
	ImageID        string            `json:"image_id"`
	CPUMillicores  int               `json:"cpu_millicores"`
	MemoryMiB      int               `json:"memory_mib"`
	DiskMiB        int               `json:"disk_mib"`
	CreatedAt      int64             `json:"created_at"`
	LastKnownState string            `json:"last_known_state"`
	Tags           map[string]string `json:"tags"`
}

type VirtualMachineDetail struct {
	ID           string            `json:"id"`
	ImageID      string            `json:"image_id"`
	Architecture string            `json:"architecture"`
	CreatedAt    int64             `json:"created_at"`
	CurrentState string            `json:"current_state"`
	DesiredState *string           `json:"desired_state"`
	Error        *string           `json:"error"`
	Tags         map[string]string `json:"tags"`
	Compute      struct {
		CPUMillicores int `json:"cpu_millicores"`
		MemoryMiB     int `json:"memory_mib"`
	} `json:"compute"`
	Disk struct {
		SizeMiB int `json:"size_mib"`
	} `json:"disk"`
	Guest struct {
		Hostname *string `json:"hostname"`
	} `json:"guest"`
	Network struct {
		MeshIPv6   *string `json:"mesh_ipv6"`
		PublicIPv4 *string `json:"public_ipv4"`
	} `json:"network"`
}

type Image struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Architecture  string `json:"architecture"`
	Status        string `json:"status"`
	Enabled       bool   `json:"enabled"`
	CreatedAt     int64  `json:"created_at"`
	RootfsSizeMiB int    `json:"rootfs_size_mib"`
}

type CreateRequest struct {
	ImageID       string            `json:"image_id"`
	CPUMillicores int               `json:"cpu_millicores"`
	MemoryMiB     int               `json:"memory_mib"`
	DiskMiB       int               `json:"disk_mib"`
	Hostname      string            `json:"hostname,omitempty"`
	Tags          map[string]string `json:"tags"`
}

type Error struct {
	Status  int
	Code    string
	Message string
	Fields  []string
}

func (err *Error) Error() string {
	return fmt.Sprintf("atlas: %d %s: %s", err.Status, err.Code, err.Detail())
}

func (err *Error) Detail() string {
	if len(err.Fields) == 0 {
		return err.Message
	}
	return err.Message + " (" + strings.Join(err.Fields, "; ") + ")"
}

func IsNotFound(err error) bool {
	var atlasError *Error
	return errors.As(err, &atlasError) && atlasError.Status == http.StatusNotFound
}

type Client struct {
	base string
	http *http.Client
}

func NewClient(base string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{
		Timeout: timeout,
		// redirects can forward credentials or turn a mutation into a get.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// atlas boots the vm during creation; stopped creation is not available.
func (client *Client) CreateVirtualMachine(ctx context.Context, caller Caller, request CreateRequest) (VirtualMachine, error) {
	var created VirtualMachine
	err := client.do(ctx, caller, http.MethodPost, "/api/atlas/virtual-machines", request, &created)
	if err == nil && created.ID == "" {
		err = fmt.Errorf("Atlas create response has no VM ID; inspect the VM list before retrying")
	}
	return created, err
}

func (client *Client) VirtualMachine(ctx context.Context, caller Caller, id string) (VirtualMachineDetail, error) {
	var detail VirtualMachineDetail
	err := client.do(ctx, caller, http.MethodGet, "/api/atlas/virtual-machines/"+url.PathEscape(id), nil, &detail)
	if err == nil && (detail.ID != id || detail.CurrentState == "") {
		err = fmt.Errorf("Atlas detail response has a missing or mismatched VM ID or state")
	}
	return detail, err
}

// reads all pages; tag uses the key:value format.
func (client *Client) VirtualMachines(ctx context.Context, caller Caller, tag string) ([]VirtualMachine, error) {
	var all []VirtualMachine
	for offset := 0; ; offset += pageLimit {
		query := url.Values{"limit": {strconv.Itoa(pageLimit)}, "offset": {strconv.Itoa(offset)}, "tag": {tag}}
		var page struct {
			Items   []VirtualMachine `json:"items"`
			HasMore *bool            `json:"has_more"`
		}
		if err := client.do(ctx, caller, http.MethodGet, "/api/atlas/virtual-machines?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		if page.Items == nil || page.HasMore == nil || (*page.HasMore && len(page.Items) != pageLimit) {
			return nil, fmt.Errorf("Atlas returned an invalid VM list page")
		}
		for _, machine := range page.Items {
			if machine.ID == "" {
				return nil, fmt.Errorf("Atlas VM list contains a missing ID")
			}
		}
		all = append(all, page.Items...)
		if !*page.HasMore {
			return all, nil
		}
	}
}

func (client *Client) Act(ctx context.Context, caller Caller, id, action string) error {
	return client.do(ctx, caller, http.MethodPost, "/api/atlas/virtual-machines/"+url.PathEscape(id)+"/actions/"+action, nil, nil)
}

func (client *Client) DeleteVirtualMachine(ctx context.Context, caller Caller, id string) error {
	return client.do(ctx, caller, http.MethodDelete, "/api/atlas/virtual-machines/"+url.PathEscape(id), nil, nil)
}

func (client *Client) Images(ctx context.Context, caller Caller) ([]Image, error) {
	var all []Image
	for offset := 0; ; offset += pageLimit {
		query := url.Values{"limit": {strconv.Itoa(pageLimit)}, "offset": {strconv.Itoa(offset)}}
		var page struct {
			Items   []Image `json:"items"`
			HasMore *bool   `json:"has_more"`
		}
		if err := client.do(ctx, caller, http.MethodGet, "/api/atlas/images?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		if page.Items == nil || page.HasMore == nil || (*page.HasMore && len(page.Items) != pageLimit) {
			return nil, fmt.Errorf("Atlas returned an invalid image list page")
		}
		for _, image := range page.Items {
			if image.ID == "" {
				return nil, fmt.Errorf("Atlas image list contains a missing ID")
			}
		}
		all = append(all, page.Items...)
		if !*page.HasMore {
			return all, nil
		}
	}
}

func (client *Client) do(ctx context.Context, caller Caller, method, path string, body, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}

	request, err := http.NewRequestWithContext(ctx, method, client.base+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", caller.Authorization)
	request.Header.Set("X-Tenant-ID", caller.TenantID)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := client.http.Do(request)
	if err != nil {
		return fmt.Errorf("atlas %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	const maximumResponseBytes = 8 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil {
		return fmt.Errorf("atlas %s %s: %w", method, path, err)
	}
	if len(data) > maximumResponseBytes {
		return fmt.Errorf("atlas %s %s: response exceeds %d bytes", method, path, maximumResponseBytes)
	}

	if response.StatusCode >= 300 {
		var answer struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Fields  []struct {
					Name    string `json:"name"`
					Message string `json:"message"`
				} `json:"fields"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &answer) != nil || answer.Error.Message == "" {
			answer.Error.Message = strings.TrimSpace(string(data))
			if len(answer.Error.Message) > 300 || answer.Error.Message == "" {
				answer.Error.Message = response.Status
			}
		}
		failure := &Error{Status: response.StatusCode, Code: answer.Error.Code, Message: answer.Error.Message}
		for _, field := range answer.Error.Fields {
			failure.Fields = append(failure.Fields, field.Name+": "+field.Message)
		}
		return failure
	}
	expectedStatus := http.StatusAccepted
	if method == http.MethodGet {
		expectedStatus = http.StatusOK
	}
	if response.StatusCode != expectedStatus {
		return fmt.Errorf("atlas %s %s: unexpected status %d", method, path, response.StatusCode)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("atlas %s %s: decode answer: %w", method, path, err)
	}
	return nil
}
