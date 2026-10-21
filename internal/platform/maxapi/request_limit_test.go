package maxapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"promo/internal/domain"
)

func TestAllMAXMethodsShareTheRequestLimit(t *testing.T) {
	const total = 60
	var mu sync.Mutex
	var requests []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/messages":
			_, _ = io.WriteString(w, `{"message":{"body":{"mid":"rate-limit-message"}}}`)
		case r.URL.Path == "/answers":
			_, _ = io.WriteString(w, `{"success":true}`)
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = io.WriteString(w, `{"members":[]}`)
		default:
			t.Errorf("unexpected MAX endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(Config{Token: "local-rate-token", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	errors := make(chan error, total)
	var workers sync.WaitGroup
	for i := range total {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			user := int64(i + 1)
			var err error
			switch i % 3 {
			case 0:
				err = client.Send(ctx, domain.Message{UserID: user, Text: "Message"})
			case 1:
				err = client.AnswerCallback(ctx, user, fmt.Sprintf("callback-%d", i))
			case 2:
				_, err = client.Subscription(ctx, -42, user)
			}
			errors <- err
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != total {
		t.Fatalf("MAX requests lost: got=%d want=%d", len(requests), total)
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].Before(requests[j]) })
	for i := 30; i < len(requests); i++ {
		if gap := requests[i].Sub(requests[i-30]); gap < time.Second-5*time.Millisecond {
			t.Fatalf("combined MAX traffic exceeded 30 requests per second: 31 requests in %v", gap)
		}
	}
}

func TestCanceledGlobalWaitDoesNotSendAnHTTPRequest(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"members":[]}`)
	}))
	defer server.Close()
	client, err := New(Config{Token: "local-rate-token", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	<-client.requests.gate
	var once sync.Once
	release := func() { once.Do(func() { client.requests.gate <- struct{}{} }) }
	t.Cleanup(release)
	for name, call := range map[string]func(context.Context) error{
		"message": func(ctx context.Context) error {
			return client.Send(ctx, domain.Message{UserID: 42, Text: "Message"})
		},
		"callback": func(ctx context.Context) error { return client.AnswerCallback(ctx, 42, "callback") },
		"membership": func(ctx context.Context) error {
			_, err := client.Subscription(ctx, -42, 42)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err := call(ctx)
			var delivery *Error
			if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &delivery) {
				t.Fatalf("unsent request received a delivery error: %v", err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("canceled requests reached MAX: %d", requests.Load())
	}
	release()
	if _, err := client.Subscription(context.Background(), -42, 42); err != nil || requests.Load() != 1 {
		t.Fatalf("canceled requests blocked later traffic: requests=%d error=%v", requests.Load(), err)
	}
}
