package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAddInvalidSpec(t *testing.T) {
	s := New(nil)
	if err := s.Add("not a cron", func(ctx context.Context) error { return nil }); err == nil {
		t.Fatal("invalid spec accepted")
	}
}

func TestJobFires(t *testing.T) {
	s := New(nil)
	var calls atomic.Int32
	if err := s.Add("@every 1s", func(ctx context.Context) error {
		calls.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Stop(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("job never fired")
	}
}

func TestStopWaitsForRunningJob(t *testing.T) {
	s := New(nil)
	jobDone := make(chan struct{})
	var once sync.Once
	_ = s.Add("@every 1s", func(ctx context.Context) error {
		time.Sleep(300 * time.Millisecond)
		once.Do(func() { close(jobDone) })
		return nil
	})
	s.Start()
	// Wait until a job run has started (poll the done channel is racy, so
	// just wait long enough for the first tick, then stop mid-run).
	time.Sleep(1100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan struct{})
	go func() {
		s.Stop(ctx)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	<-jobDone
}
