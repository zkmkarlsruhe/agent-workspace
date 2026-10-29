// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestShutdownJoinsLifecycleLoopAndHTTPBeforeClosingOffice(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		r, err := http.Get(srv.URL)
		if err == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
	}()
	<-entered
	closed := make(chan struct{})
	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go shutdownManager(deadline, []*http.Server{srv.Config}, stop, loopDone, func() { close(closed) })
	<-ctx.Done()
	select {
	case <-closed:
		t.Fatal("closed lifecycle while HTTP active")
	default:
	}
	close(release)
	<-requestDone
	select {
	case <-closed:
		t.Fatal("closed lifecycle before loop joined")
	default:
	}
	close(loopDone)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("lifecycle cleanup not invoked")
	}
}
func TestShutdownCancelsOverdueHTTPAndStillClosesLifecycle(t *testing.T) {
	entered := make(chan context.Context, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- r.Context(); <-r.Context().Done() }))
	defer srv.Close()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		r, err := http.Get(srv.URL)
		if err == nil {
			r.Body.Close()
		}
	}()
	requestCtx := <-entered
	deadline, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	stopped, closed := false, false
	shutdownManager(deadline, []*http.Server{srv.Config}, func() { stopped = true }, nil, func() { closed = true })
	if !stopped || !closed {
		t.Fatal("lifecycle cleanup skipped")
	}
	select {
	case <-requestCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("HTTP request survived deadline")
	}
	<-requestDone
}
