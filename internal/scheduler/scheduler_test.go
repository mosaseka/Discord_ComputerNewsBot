package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCoordinatorExcludesConcurrentRuns(t *testing.T) {
	var c Coordinator
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = c.TryRun(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	err := c.TryRun(context.Background(), func(context.Context) error { return nil })
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("TryRun() error = %v, want ErrBusy", err)
	}
	close(release)
	wg.Wait()
}

func TestFiveFieldCronAndTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), "0 8 * * *", loc, func(context.Context) {})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Shutdown()
	s.Start()
	next, err := s.NextRun()
	if err != nil {
		t.Fatalf("NextRun() error = %v", err)
	}
	if next.In(loc).Hour() != 8 || next.In(loc).Minute() != 0 {
		t.Fatalf("next run = %v, want 08:00 JST", next.In(loc))
	}
}

func TestRejectsSixFieldCron(t *testing.T) {
	_, err := New(context.Background(), "0 0 8 * * *", time.UTC, func(context.Context) {})
	if err == nil {
		t.Fatal("New() accepted a six-field cron expression")
	}
}
