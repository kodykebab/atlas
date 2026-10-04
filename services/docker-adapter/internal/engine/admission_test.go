package engine

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestUnsupportedCreateOptionsNeverReachAtlas(t *testing.T) {
	server := newEngineServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("rejected request reached Atlas")
		w.WriteHeader(500)
	})
	for _, body := range []string{
		`{"Image":"ubuntu","AttachStdout":true}`, `{"Image":"ubuntu","AttachStderr":true}`,
		`{"Image":"ubuntu","StopTimeout":0}`,
		`{"Image":"ubuntu","FutureField":null}`, `{"Image":"ubuntu","Cmd":[""]}`,
		`{"Image":"ubuntu","HostConfig":{"DeviceRequests":[{"Count":-1,"Capabilities":[["gpu"]]}]}}`,
		`{"Image":"ubuntu","HostConfig":{"DnsSearch":["example.test"]}}`,
		`{"Image":"ubuntu","HostConfig":{"StorageOpt":{"size":"20G"}}}`,
		`{"Image":"ubuntu","HostConfig":{"MemorySwap":-1}}`,
		`{"Image":"ubuntu","HostConfig":{"MemorySwappiness":0}}`,
		`{"Image":"ubuntu","HostConfig":{"PidsLimit":-1}}`,
		`{"Image":"ubuntu","HostConfig":{"Init":true}}`,
		`{"Image":"ubuntu","HostConfig":{"FutureOption":false}}`,
		`{"Image":"ubuntu","NetworkingConfig":{"EndpointsConfig":{"default":{"IPAddress":"10.0.0.4"}}}}`,
		`{"Image":"ubuntu","NetworkingConfig":{"EndpointsConfig":{"custom":{}}}}`,
		`{"Image":"ubuntu","HostConfig":{"RestartPolicy":{"Name":"no","MaximumRetryCount":3}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			response := serveRequest(server, "POST", "/containers/create", body)
			if response.Code != 400 {
				t.Fatalf("got %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestRecordedDetachedCreateDefaults(t *testing.T) {
	file, err := os.Open("../../testdata/cli-29.4.1-requests.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	checked := 0
	for scanner.Scan() {
		var record struct {
			Path string          `json:"path"`
			Body json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(record.Path, "/containers/create") {
			continue
		}
		var attachment struct{ AttachStdout, AttachStderr bool }
		json.Unmarshal(record.Body, &attachment)
		if attachment.AttachStdout || attachment.AttachStderr {
			continue
		}
		if _, err := decodeCreate(strings.NewReader(string(record.Body))); err != nil {
			t.Fatalf("recorded defaults: %v", err)
		}
		checked++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no detached creation checked")
	}
}

func TestUnsupportedQueriesAndRestartNeverReachAtlas(t *testing.T) {
	server := newEngineServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("rejected request reached Atlas")
		w.WriteHeader(500)
	})
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{"POST", "/containers/create?platform=linux/arm64", 400},
		{"POST", "/containers/create?name=a&name=b", 400},
		{"GET", "/containers/json?limit=1", 400},
		{"GET", "/containers/json?size=true", 400},
		{"GET", "/images/json?filters=%7B%22reference%22:%5B%22ubuntu%22%5D%7D", 400},
		{"POST", "/containers/web/restart", 501},
	} {
		response := serveRequest(server, test.method, test.path, `{"Image":"ubuntu"}`)
		if response.Code != test.status {
			t.Fatalf("%s: %d %s", test.path, response.Code, response.Body.String())
		}
	}
}
