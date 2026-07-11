package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-co-op/gocron/v2"
)

var ErrBusy = errors.New("briefing run is already in progress")

type Coordinator struct {
	busy atomic.Bool
}

func (c *Coordinator) TryRun(ctx context.Context, fn func(context.Context) error) error {
	if !c.busy.CompareAndSwap(false, true) {
		return ErrBusy
	}
	defer c.busy.Store(false)
	return fn(ctx)
}

func (c *Coordinator) Busy() bool { return c.busy.Load() }

type Scheduler struct {
	scheduler gocron.Scheduler
	job       gocron.Job
}

func New(ctx context.Context, expression string, location *time.Location, task func(context.Context)) (*Scheduler, error) {
	if ctx == nil {
		return nil, errors.New("scheduler context is nil")
	}
	if location == nil {
		return nil, errors.New("scheduler location is nil")
	}
	if task == nil {
		return nil, errors.New("scheduler task is nil")
	}
	s, err := gocron.NewScheduler(gocron.WithLocation(location))
	if err != nil {
		return nil, fmt.Errorf("create scheduler: %w", err)
	}
	job, err := s.NewJob(
		gocron.CronJob(expression, false),
		gocron.NewTask(func() { task(ctx) }),
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
	)
	if err != nil {
		s.Shutdown()
		return nil, fmt.Errorf("create five-field cron job %q: %w", expression, err)
	}
	return &Scheduler{scheduler: s, job: job}, nil
}

func (s *Scheduler) Start() {
	if s != nil && s.scheduler != nil {
		s.scheduler.Start()
	}
}

func (s *Scheduler) NextRun() (time.Time, error) {
	if s == nil || s.job == nil {
		return time.Time{}, errors.New("scheduler is not initialized")
	}
	return s.job.NextRun()
}

func (s *Scheduler) Shutdown() error {
	if s == nil || s.scheduler == nil {
		return nil
	}
	return s.scheduler.Shutdown()
}
