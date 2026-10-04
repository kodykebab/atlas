package atlas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMalformedSuccessResponses(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		call       func(*Client) error
	}{
		{"create without ID", `{}`, 202, func(client *Client) error {
			_, err := client.CreateVirtualMachine(context.Background(), Caller{}, CreateRequest{})
			return err
		}},
		{"null detail", `null`, 200, func(client *Client) error {
			_, err := client.VirtualMachine(context.Background(), Caller{}, "vm-1")
			return err
		}},
		{"wrong detail ID", `{"id":"vm-other","current_state":"running"}`, 200, func(client *Client) error {
			_, err := client.VirtualMachine(context.Background(), Caller{}, "vm-1")
			return err
		}},
		{"mutation without acceptance", ``, 204, func(client *Client) error {
			return client.Act(context.Background(), Caller{}, "vm-1", "start")
		}},
		{"oversized mutation response", strings.Repeat(" ", (8<<20)+1), 202, func(client *Client) error {
			return client.DeleteVirtualMachine(context.Background(), Caller{}, "vm-1")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(test.status)
				fmt.Fprint(writer, test.body)
			}))
			defer backend.Close()
			if err := test.call(NewClient(backend.URL, time.Second)); err == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
}

func TestInvalidListPages(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"items":[]}`, `{"items":null,"has_more":false}`, `{"items":[],"has_more":true}`, `{"items":[{}],"has_more":false}`} {
		t.Run(body, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				fmt.Fprint(writer, body)
			}))
			defer backend.Close()
			client := NewClient(backend.URL, time.Second)
			if _, err := client.VirtualMachines(context.Background(), Caller{}, "docker.managed:true"); err == nil {
				t.Fatal("invalid VM page accepted")
			}
			if _, err := client.Images(context.Background(), Caller{}); err == nil {
				t.Fatal("invalid image page accepted")
			}
		})
	}
}

func TestPaginationPreservesCallerAndReportsPartialFailure(t *testing.T) {
	for _, failSecondPage := range []bool{false, true} {
		t.Run(strconv.FormatBool(failSecondPage), func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer caller" || request.Header.Get("X-Tenant-ID") != "23" {
					t.Error("caller credentials missing")
				}
				query := request.URL.Query()
				if query.Get("limit") != "100" || (request.URL.Path == "/api/atlas/virtual-machines" && query.Get("tag") != "docker.managed:true") {
					t.Errorf("unexpected query: %s", request.URL.RawQuery)
				}
				offset, _ := strconv.Atoi(query.Get("offset"))
				if offset != 0 && offset != 100 && offset != 200 {
					t.Errorf("unexpected offset %d", offset)
					writer.WriteHeader(400)
					return
				}
				if offset == 100 && failSecondPage {
					writer.WriteHeader(403)
					fmt.Fprint(writer, `{"error":{"message":"Permission denied"}}`)
					return
				}
				items := []map[string]string{}
				for index := offset; index < min(offset+100, 205); index++ {
					items = append(items, map[string]string{"id": fmt.Sprintf("item-%d", index)})
				}
				json.NewEncoder(writer).Encode(map[string]any{"items": items, "has_more": offset < 200})
			}))
			defer backend.Close()
			client := NewClient(backend.URL, time.Second)
			caller := Caller{Authorization: "Bearer caller", TenantID: "23"}
			machines, machineError := client.VirtualMachines(context.Background(), caller, "docker.managed:true")
			images, imageError := client.Images(context.Background(), caller)
			if failSecondPage {
				if machineError == nil || imageError == nil || machines != nil || images != nil {
					t.Fatal("partial page returned as a successful list")
				}
			} else if machineError != nil || imageError != nil || len(machines) != 205 || len(images) != 205 {
				t.Fatalf("pagination: VMs=%d (%v), images=%d (%v)", len(machines), machineError, len(images), imageError)
			}
		})
	}
}

func TestRedirectDoesNotForwardCredentialsOrRepeatMutation(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	for _, status := range []int{301, 302, 307, 308} {
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, status) }))
		err := NewClient(source.URL, time.Second).DeleteVirtualMachine(context.Background(), Caller{Authorization: "Bearer secret", TenantID: "7"}, "vm-1")
		source.Close()
		var failure *Error
		if !errors.As(err, &failure) || failure.Status != status {
			t.Fatalf("redirect %d: %v", status, err)
		}
	}
	if redirected.Load() != 0 {
		t.Fatalf("followed %d redirects", redirected.Load())
	}
}
