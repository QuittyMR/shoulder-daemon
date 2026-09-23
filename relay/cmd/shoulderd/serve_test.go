package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type served struct {
	url    string
	stop   context.CancelFunc
	ran    chan struct{}
	result chan error
}

// startServing runs serveUntil on a free port with handler, as serve does,
// with a pipeline whose end the test decides by closing ran.
func startServing(t *testing.T, handler http.Handler, reqWait, runWait time.Duration) *served {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	s := &served{url: "http://" + ln.Addr().String(), stop: stop, ran: make(chan struct{}), result: make(chan error, 1)}
	hs := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { s.result <- serveUntil(ctx, stop, hs, ln, s.ran, reqWait, runWait, log) }()
	return s
}

func (s *served) returned(t *testing.T, within time.Duration) bool {
	t.Helper()
	select {
	case err := <-s.result:
		if err != nil {
			t.Fatalf("serveUntil: %v", err)
		}
		return true
	case <-time.After(within):
		return false
	}
}

// The store is closed the moment serveUntil returns, so returning while a
// request or the pipeline's last sweep is still writing to it is the bug.
func TestServeUntilWaitsForRequestsAndThePipeline(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s := startServing(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}), 5*time.Second, 5*time.Second)

	answered := make(chan error, 1)
	go func() {
		resp, err := http.Get(s.url)
		if err == nil {
			_ = resp.Body.Close()
		}
		answered <- err
	}()
	<-entered
	s.stop()

	if s.returned(t, 50*time.Millisecond) {
		t.Fatal("returned with a request still in flight")
	}
	close(release)
	if err := <-answered; err != nil {
		t.Fatalf("the request in flight was cut off: %v", err)
	}
	if s.returned(t, 50*time.Millisecond) {
		t.Fatal("returned with the pipeline still running")
	}
	close(s.ran)
	if !s.returned(t, 5*time.Second) {
		t.Fatal("did not return once the request and the pipeline had finished")
	}
}

// A learn or a tidying pass can ask the model for minutes, far past the grace a
// stopping daemon gives requests. Stopping has to reach it as a cancellation,
// so it gives up the question and writes down what it had already decided
// while the store is still open.
func TestServeUntilCancelsRequestsSoTheyCanFinishInTime(t *testing.T) {
	entered := make(chan struct{})
	var wrote atomic.Bool
	s := startServing(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		time.Sleep(20 * time.Millisecond)
		wrote.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}), 5*time.Second, 5*time.Second)
	close(s.ran)

	go func() {
		if resp, err := http.Get(s.url); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	s.stop()
	if !s.returned(t, 2*time.Second) {
		t.Fatal("the request in flight was never told the daemon is stopping")
	}
	if !wrote.Load() {
		t.Fatal("returned before the cancelled request had written what it decided")
	}
}

// A request that never finishes and a pipeline stuck on one must not keep a
// daemon that was told to stop alive.
func TestServeUntilGivesUpOnWhatDoesNotFinish(t *testing.T) {
	entered, hung := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(hung) })
	s := startServing(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-hung
	}), 50*time.Millisecond, 50*time.Millisecond)

	go func() {
		if resp, err := http.Get(s.url); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	s.stop()
	if !s.returned(t, 5*time.Second) {
		t.Fatal("waited past its grace for a request and a pipeline that never finish")
	}
}

// A listener that fails on its own is not a signal, so nothing else has told
// the pipeline to stop; serveUntil must, or it waits out its grace for nothing
// and closes the store under a pipeline that is still running.
func TestServeUntilStopsThePipelineWhenTheListenerFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	ran := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(ran)
	}()
	hs := &http.Server{ReadHeaderTimeout: time.Second}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	result := make(chan error, 1)
	go func() { result <- serveUntil(ctx, stop, hs, ln, ran, time.Minute, time.Minute, log) }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("a listener that failed was reported as a clean stop")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waited out its grace instead of stopping the pipeline")
	}
}
