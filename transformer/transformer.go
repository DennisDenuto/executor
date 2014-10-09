package transformer

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/cloudfoundry-incubator/executor/api"
	"github.com/cloudfoundry-incubator/executor/log_streamer"
	"github.com/cloudfoundry-incubator/executor/sequence"
	"github.com/cloudfoundry-incubator/executor/steps/download_step"
	"github.com/cloudfoundry-incubator/executor/steps/emit_progress_step"
	"github.com/cloudfoundry-incubator/executor/steps/fetch_result_step"
	"github.com/cloudfoundry-incubator/executor/steps/monitor_step"
	"github.com/cloudfoundry-incubator/executor/steps/parallel_step"
	"github.com/cloudfoundry-incubator/executor/steps/run_step"
	"github.com/cloudfoundry-incubator/executor/steps/try_step"
	"github.com/cloudfoundry-incubator/executor/steps/upload_step"
	"github.com/cloudfoundry-incubator/executor/uploader"
	garden_api "github.com/cloudfoundry-incubator/garden/api"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/dropsonde/emitter/logemitter"
	"github.com/pivotal-golang/archiver/compressor"
	"github.com/pivotal-golang/archiver/extractor"
	"github.com/pivotal-golang/cacheddownloader"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/timer"
)

var ErrNoCheck = errors.New("no check configured")

type Transformer struct {
	logEmitter       logemitter.Emitter
	gardenClient     garden_api.Client
	cachedDownloader cacheddownloader.CachedDownloader
	uploader         uploader.Uploader
	extractor        extractor.Extractor
	compressor       compressor.Compressor
	logger           lager.Logger
	tempDir          string
	result           *string
}

func NewTransformer(
	logEmitter logemitter.Emitter,
	gardenClient garden_api.Client,
	cachedDownloader cacheddownloader.CachedDownloader,
	uploader uploader.Uploader,
	extractor extractor.Extractor,
	compressor compressor.Compressor,
	logger lager.Logger,
	tempDir string,
) *Transformer {
	return &Transformer{
		logEmitter:       logEmitter,
		gardenClient:     gardenClient,
		cachedDownloader: cachedDownloader,
		uploader:         uploader,
		extractor:        extractor,
		compressor:       compressor,
		logger:           logger,
		tempDir:          tempDir,
	}
}

func (transformer *Transformer) StepsFor(
	logConfig api.LogConfig,
	actions []models.ExecutorAction,
	globalEnv []api.EnvironmentVariable,
	container garden_api.Container,
	result *string,
) ([]sequence.Step, error) {
	subSteps := []sequence.Step{}

	for _, a := range actions {
		step, err := transformer.convertAction(logConfig, a, globalEnv, container, result)
		if err != nil {
			return nil, err
		}

		subSteps = append(subSteps, step)
	}

	return subSteps, nil
}

func (transformer *Transformer) convertAction(
	logConfig api.LogConfig,
	action models.ExecutorAction,
	globalEnv []api.EnvironmentVariable,
	container garden_api.Container,
	result *string,
) (sequence.Step, error) {
	logStreamer := log_streamer.New(logConfig.Guid, logConfig.SourceName, logConfig.Index, transformer.logEmitter)

	logger := transformer.logger.WithData(lager.Data{
		"handle": container.Handle(),
	})

	switch actionModel := action.Action.(type) {
	case models.RunAction:
		var runEnv []models.EnvironmentVariable
		for _, e := range globalEnv {
			runEnv = append(runEnv, models.EnvironmentVariable{
				Name:  e.Name,
				Value: e.Value,
			})
		}

		actionModel.Env = append(runEnv, actionModel.Env...)

		return run_step.New(
			container,
			actionModel,
			logStreamer,
			logger,
		), nil
	case models.DownloadAction:
		return download_step.New(
			transformer.gardenClient,
			container,
			actionModel,
			transformer.cachedDownloader,
			transformer.extractor,
			transformer.tempDir,
			logger,
		), nil
	case models.UploadAction:
		return upload_step.New(
			container,
			actionModel,
			transformer.uploader,
			transformer.compressor,
			transformer.tempDir,
			logStreamer,
			logger,
		), nil
	case models.FetchResultAction:
		return fetch_result_step.New(
			container,
			actionModel,
			transformer.tempDir,
			logger,
			result,
		), nil
	case models.EmitProgressAction:
		subStep, err := transformer.convertAction(
			logConfig,
			actionModel.Action,
			globalEnv,
			container,
			result,
		)
		if err != nil {
			return nil, err
		}

		return emit_progress_step.New(
			subStep,
			actionModel.StartMessage,
			actionModel.SuccessMessage,
			actionModel.FailureMessage,
			logStreamer,
			logger,
		), nil
	case models.TryAction:
		subStep, err := transformer.convertAction(
			logConfig,
			actionModel.Action,
			globalEnv,
			container,
			result,
		)
		if err != nil {
			return nil, err
		}

		return try_step.New(subStep, logger), nil
	case models.MonitorAction:
		var healthyHook *http.Request
		var unhealthyHook *http.Request

		if actionModel.HealthyHook.URL != "" {
			healthyHookURL, err := url.ParseRequestURI(actionModel.HealthyHook.URL)
			if err != nil {
				return nil, err
			}

			healthyHook = &http.Request{
				Method: actionModel.HealthyHook.Method,
				URL:    healthyHookURL,
			}
		}

		if actionModel.UnhealthyHook.URL != "" {
			unhealthyHookURL, err := url.ParseRequestURI(actionModel.UnhealthyHook.URL)
			if err != nil {
				return nil, err
			}

			unhealthyHook = &http.Request{
				Method: actionModel.UnhealthyHook.Method,
				URL:    unhealthyHookURL,
			}
		}

		check, err := transformer.convertAction(
			logConfig,
			actionModel.Action,
			globalEnv,
			container,
			result,
		)
		if err != nil {
			return nil, err
		}

		return monitor_step.New(
			check,
			actionModel.HealthyThreshold,
			actionModel.UnhealthyThreshold,
			healthyHook,
			unhealthyHook,
			logger,
			timer.NewTimer(),
		), nil
	case models.ParallelAction:
		steps := make([]sequence.Step, len(actionModel.Actions))
		for i, action := range actionModel.Actions {
			var err error

			steps[i], err = transformer.convertAction(
				logConfig,
				action,
				globalEnv,
				container,
				result,
			)
			if err != nil {
				return nil, err
			}
		}

		return parallel_step.New(steps), nil
	}

	panic(fmt.Sprintf("unknown action: %T", action))
}
