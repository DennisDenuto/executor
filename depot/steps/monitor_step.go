package steps

import (
	"fmt"
	"time"

	"github.com/cloudfoundry-incubator/executor/depot/log_streamer"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/pivotal-golang/lager"
)

func invalidInterval(field string, interval time.Duration) error {
	return fmt.Errorf("The %s interval, %s, is not positive.", field, interval.String())
}

const timeoutMessage = "Timed out after %s: health check never passed.\n"

type monitorStep struct {
	checkFunc         func() Step
	hasStartedRunning chan<- struct{}

	logger       lager.Logger
	timeProvider timeprovider.TimeProvider
	logStreamer  log_streamer.LogStreamer

	startTimeout      time.Duration
	healthyInterval   time.Duration
	unhealthyInterval time.Duration

	*canceller
}

func NewMonitor(
	checkFunc func() Step,
	hasStartedRunning chan<- struct{},
	logger lager.Logger,
	timeProvider timeprovider.TimeProvider,
	logStreamer log_streamer.LogStreamer,
	startTimeout time.Duration,
	healthyInterval time.Duration,
	unhealthyInterval time.Duration,
) Step {
	logger = logger.Session("monitor-step")

	return &monitorStep{
		checkFunc:         checkFunc,
		hasStartedRunning: hasStartedRunning,
		logger:            logger,
		timeProvider:      timeProvider,
		logStreamer:       logStreamer,
		startTimeout:      startTimeout,
		healthyInterval:   healthyInterval,
		unhealthyInterval: unhealthyInterval,

		canceller: newCanceller(),
	}
}

func (step *monitorStep) Perform() error {
	if step.healthyInterval <= 0 {
		return invalidInterval("healthy", step.healthyInterval)
	}

	if step.unhealthyInterval <= 0 {
		return invalidInterval("unhealthy", step.unhealthyInterval)
	}

	healthy := false
	interval := step.unhealthyInterval

	var startBy *time.Time
	if step.startTimeout > 0 {
		t := step.timeProvider.Now().Add(step.startTimeout)
		startBy = &t
	}

	timer := step.timeProvider.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case now := <-timer.C():
			stepResult := make(chan error)

			check := step.checkFunc()

			go func() {
				stepResult <- check.Perform()
			}()

			select {
			case stepErr := <-stepResult:
				nowHealthy := stepErr == nil

				if healthy && !nowHealthy {
					step.logger.Info("transitioned-to-unhealthy")
					return stepErr
				} else if !healthy && nowHealthy {
					step.logger.Info("transitioned-to-healthy")
					healthy = true
					step.hasStartedRunning <- struct{}{}
					interval = step.healthyInterval
					startBy = nil
				}

				if startBy != nil && now.After(*startBy) {
					if !healthy {
						fmt.Fprintf(step.logStreamer.Stderr(), timeoutMessage, step.startTimeout)
						step.logger.Info("timed-out-before-healthy")
						return stepErr
					}

					startBy = nil
				}

			case <-step.Cancelled():
				check.Cancel()
				return <-stepResult
			}

		case <-step.Cancelled():
			return ErrCancelled
		}

		timer.Reset(interval)
	}

	panic("unreachable")
}
