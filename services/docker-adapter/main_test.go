package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestInvalidStartupConfiguration(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, arguments := range [][]string{
		{}, {"-atlas-url", "http://:123"}, {"-atlas-url", "ftp://example.test"}, {"-atlas-url", "http://user:password@example.test"},
		{"-atlas-url", "http://example.test/path"}, {"-atlas-url", "http://example.test?token=secret"},
		{"-atlas-url", "http://example.test", "-poll-interval", "0s"},
		{"-atlas-url", "http://example.test", "-operation-timeout", "-1s"},
		{"-atlas-url", "http://example.test", "-cpu-millicores", "0"},
		{"-atlas-url", "http://example.test", "-memory-mib", "0"},
		{"-atlas-url", "http://example.test", "-disk-mib", "-1"},
	} {
		if err := run(context.Background(), arguments, logger); err == nil {
			t.Fatalf("accepted %v", arguments)
		}
	}
}

func TestShutdownCancelsAndWaitsForActiveHandler(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	result := make(chan error, 1)
	go func() {
		result <- serve(ctx, listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
			close(canceled)
			<-release
			w.WriteHeader(204)
		}))
	}()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		response, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler never started")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("request not canceled")
	}
	select {
	case err := <-result:
		t.Fatalf("serve returned before handler: %v", err)
	default:
	}
	release <- struct{}{}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown hung")
	}
	<-clientDone
}
