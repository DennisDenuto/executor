package steps

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"time"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/executor/depot/log_streamer"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/workpool"
)

type longRunningMonitorStep struct {
	hasStartedRunning chan<- struct{}

	readinessCheck func(log_streamer.LogStreamer) Step
	livenessCheck  func(log_streamer.LogStreamer) Step

	logger                           lager.Logger
	clock                            clock.Clock
	logStreamer, healthCheckStreamer log_streamer.LogStreamer

	startTimeout time.Duration
	workPool     *workpool.WorkPool

	*canceller
}

func NewLongRunningMonitor(
	readinessCheckFunc func(log_streamer.LogStreamer) Step,
	livenessCheckFunc func(log_streamer.LogStreamer) Step,
	hasStartedRunning chan<- struct{},
	logger lager.Logger,
	clock clock.Clock,
	logStreamer log_streamer.LogStreamer,
	startTimeout time.Duration,
	workPool *workpool.WorkPool,
) Step {
	logger = logger.Session("monitor-step")

	return &longRunningMonitorStep{
		readinessCheck:      readinessCheckFunc,
		livenessCheck:       livenessCheckFunc,
		hasStartedRunning:   hasStartedRunning,
		logger:              logger,
		clock:               clock,
		logStreamer:         logStreamer,
		startTimeout:        startTimeout,
		healthCheckStreamer: logStreamer.WithSource("HEALTH"),

		canceller: newCanceller(),
		workPool:  workPool,
	}
}

func (step *longRunningMonitorStep) Perform() error {
	fmt.Fprint(step.logStreamer.Stdout(), "Starting health monitoring of container\n")

	errCh := make(chan error)

	var outBuffer bytes.Buffer
	bufferStreamer := log_streamer.NewBufferStreamer(&outBuffer, ioutil.Discard)
	readinessCheck := step.readinessCheck(bufferStreamer)

	step.workPool.Submit(func() {
		errCh <- readinessCheck.Perform()
	})

	timer := step.clock.NewTimer(step.startTimeout)

	select {
	case <-errCh:
	case <-timer.C():
		readinessCheck.Cancel()
		err := <-errCh
		fmt.Fprintf(step.healthCheckStreamer.Stderr(), "%s\n", outBuffer.String())
		fmt.Fprintf(step.logStreamer.Stderr(), timeoutMessage, step.startTimeout)
		step.logger.Info("timed-out-before-healthy")
		return NewEmittableError(err, healthcheckNowUnhealthy, outBuffer.String())
	case <-step.cancelled:
		readinessCheck.Cancel()
		return <-errCh
	}

	step.logger.Info("transitioned-to-healthy")
	fmt.Fprint(step.logStreamer.Stdout(), "Container became healthy\n")
	step.hasStartedRunning <- struct{}{}

	livenessCheck := step.livenessCheck(bufferStreamer)
	step.workPool.Submit(func() {
		errCh <- livenessCheck.Perform()
	})

	select {
	case err := <-errCh:
		step.logger.Info("transitioned-to-unhealthy")
		fmt.Fprint(step.logStreamer.Stdout(), "Container became unhealthy\n")
		fmt.Fprintf(step.healthCheckStreamer.Stderr(), "%s\n", outBuffer.String())
		return NewEmittableError(err, healthcheckNowUnhealthy, outBuffer.String())
	case <-step.cancelled:
		livenessCheck.Cancel()
		return <-errCh
	}
}
