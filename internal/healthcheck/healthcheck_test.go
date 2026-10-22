package healthcheck

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadinessURLUsesTheConfiguredListener(t *testing.T) {
	for _, test := range []struct{ address, want string }{
		{":8080", "http://127.0.0.1:8080/readyz"},
		{"0.0.0.0:8123", "http://127.0.0.1:8123/readyz"},
		{"127.0.0.2:8081", "http://127.0.0.2:8081/readyz"},
		{"service.local:9000", "http://service.local:9000/readyz"},
		{"[::]:8080", "http://[::1]:8080/readyz"},
		{"[0:0:0:0:0:0:0:0]:8080", "http://[::1]:8080/readyz"},
		{"[::ffff:0.0.0.0]:8080", "http://127.0.0.1:8080/readyz"},
		{"[0:0:0:0:0:ffff:0:0]:8080", "http://127.0.0.1:8080/readyz"},
		{"[::1]:80", "http://[::1]:80/readyz"},
		{"[2001:db8::4]:65535", "http://[2001:db8::4]:65535/readyz"},
		{"[fe80::1%en0]:8080", "http://[fe80::1%25en0]:8080/readyz"},
		{"localhost:00080", "http://localhost:80/readyz"},
	} {
		t.Run(test.address, func(t *testing.T) {
			got, err := readinessURL(test.address)
			if err != nil || got != test.want {
				t.Fatalf("readiness URL=%q error=%v, want=%q", got, err, test.want)
			}
		})
	}
}

func TestReadinessURLRejectsMalformedAddressesWithoutReflectingInput(t *testing.T) {
	for _, address := range []string{
		"", "secret", "secret:", "secret:0", "secret:-1", "secret:+80", "secret:65536", "secret:http", "secret:80 ",
		"http://secret:80", "secret@localhost:80", "secret/path:80", "secret?query:80", "secret#fragment:80", "secret\\host:80",
		"secret host:80", "[secret:host]:80", "secret%zone:80", "::1:80", "[::1]", "[::1]:0",
	} {
		t.Run(address, func(t *testing.T) {
			_, err := readinessURL(address)
			if err == nil || err.Error() != "readiness address must use host:port with a port from 1 to 65535" {
				t.Fatalf("malformed address accepted or reflected in the diagnostic: %v", err)
			}
		})
	}
}

func TestCheckUsesGETReadinessAtTheConfiguredPort(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/readyz" {
			t.Errorf("unexpected readiness request: %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"127.0.0.1", "", "0.0.0.0"} {
		if err := Check(context.Background(), net.JoinHostPort(host, port)); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("readiness requests=%d, want=3", calls.Load())
	}
}

func TestCheckRequiresStatus200(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusNoContent, http.StatusNotFound, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			if err := Check(context.Background(), server.Listener.Addr().String()); err == nil {
				t.Fatalf("non-200 readiness status accepted: %v", err)
			}
		})
	}
}

func TestCheckDoesNotFollowRedirects(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, upstream.URL, http.StatusFound)
	}))
	defer server.Close()
	if err := Check(context.Background(), server.Listener.Addr().String()); err == nil || upstreamCalls.Load() != 0 {
		t.Fatalf("readiness redirect was followed: error=%v upstream calls=%d", err, upstreamCalls.Load())
	}
}

func TestCheckBypassesTheEnvironmentProxy(t *testing.T) {
	var proxyCalls, directCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer proxy.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	for _, name := range []string{"HTTP_PROXY", "http_proxy"} {
		t.Setenv(name, proxy.URL)
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	address := net.JoinHostPort("localhost.", port)
	endpoint, err := readinessURL(address)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	configuredProxy, err := http.ProxyFromEnvironment(request)
	if err != nil || configuredProxy == nil {
		t.Fatal("fixture does not require bypassing the environment proxy")
	}
	if err := Check(context.Background(), address); err != nil {
		t.Fatal(err)
	}
	if proxyCalls.Load() != 0 || directCalls.Load() != 1 {
		t.Fatalf("readiness used the proxy: proxy=%d direct=%d", proxyCalls.Load(), directCalls.Load())
	}
}

func TestCheckTimesOutWithinTwoSeconds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	started := time.Now()
	err := Check(context.Background(), server.Listener.Addr().String())
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) || elapsed < 1500*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("readiness timeout was not bounded: elapsed=%s error=%v", elapsed, err)
	}
}

func TestCheckStopsWhenItsContextIsCanceled(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Check(ctx, server.Listener.Addr().String()) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("readiness request did not reach the server")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("context cancellation was lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("readiness ignored context cancellation")
	}
}

func TestCheckRejectsInvalidAddressBeforeMakingARequest(t *testing.T) {
	err := Check(context.Background(), "secret@localhost:8080")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("invalid readiness address accepted or exposed: %v", err)
	}
}
