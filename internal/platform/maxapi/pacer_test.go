package maxapi

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestPacerCancellationDoesNotWaitForAnotherCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pace := newPacer(time.Hour)
		<-pace.gate
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		if err := pace.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiting for gate ignored context: %v", err)
		}
		pace.gate <- struct{}{}
		if err := pace.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		canceled, cancelNow := context.WithCancel(context.Background())
		cancelNow()
		if err := pace.wait(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled pacing succeeded: %v", err)
		}
	})
}

func TestRecipientPacingKeepsDialogsIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pace := newRecipientPacer(time.Hour)
		if err := pace.wait(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		waiting := make(chan error, 1)
		go func() { waiting <- pace.wait(ctx, 1) }()
		synctest.Wait()
		if err := pace.wait(ctx, 2); err != nil {
			t.Fatalf("busy dialog blocked another recipient: %v", err)
		}
		if err := <-waiting; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("same dialog bypassed its rate limit: %v", err)
		}
	})
}

func TestRecipientPacingReclaimsIdleRecipients(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pace := newRecipientPacer(5 * time.Millisecond)
		for recipient := range int64(1000) {
			if err := pace.wait(context.Background(), recipient); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(15 * time.Millisecond)
		if err := pace.wait(context.Background(), 1001); err != nil {
			t.Fatal(err)
		}
		pace.mu.Lock()
		defer pace.mu.Unlock()
		if len(pace.entries) != 1 {
			t.Fatalf("idle recipient limiters retained: %d", len(pace.entries))
		}
	})
}

func TestRecipientIntervalStartsAfterTheGlobalAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		global := newPacer(time.Nanosecond)
		global.next = time.Now().Add(100 * time.Millisecond)
		recipient := newPacer(50 * time.Millisecond)
		recipient.parent = global
		if err := recipient.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if err := recipient.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("global wait consumed the recipient interval: %v", err)
		}
	})
}
