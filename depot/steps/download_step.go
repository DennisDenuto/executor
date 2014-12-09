package steps

import (
	"fmt"
	"io"
	"net/url"

	"github.com/cloudfoundry-incubator/executor/depot/log_streamer"
	garden_api "github.com/cloudfoundry-incubator/garden/api"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/pivotal-golang/bytefmt"
	"github.com/pivotal-golang/cacheddownloader"
	"github.com/pivotal-golang/lager"
)

type downloadStep struct {
	container        garden_api.Container
	model            models.DownloadAction
	cachedDownloader cacheddownloader.CachedDownloader
	streamer         log_streamer.LogStreamer
	rateLimiter      chan struct{}
	cancelChan       chan struct{}
	logger           lager.Logger
}

func NewDownload(
	container garden_api.Container,
	model models.DownloadAction,
	cachedDownloader cacheddownloader.CachedDownloader,
	rateLimiter chan struct{},
	streamer log_streamer.LogStreamer,
	logger lager.Logger,
) *downloadStep {
	logger = logger.Session("download-step", lager.Data{
		"to":       model.To,
		"cacheKey": model.CacheKey,
	})

	cancelChan := make(chan struct{})

	return &downloadStep{
		container:        container,
		model:            model,
		cachedDownloader: cachedDownloader,
		streamer:         streamer,
		rateLimiter:      rateLimiter,
		cancelChan:       cancelChan,
		logger:           logger,
	}
}

func (step *downloadStep) Cancel() {
	close(step.cancelChan)
}

func (step *downloadStep) Perform() error {
	select {
	case step.rateLimiter <- struct{}{}:
	case <-step.cancelChan:
		return CancelError{}
	}

	defer func() {
		<-step.rateLimiter
	}()

	step.logger.Info("starting-download")
	step.emit("Downloading %s...\n", step.model.Artifact)

	downloadedFile, err := step.download()
	if err != nil {
		select {
		case <-step.cancelChan:
			return CancelError{}
		default:
			step.emit("Failed to download %s\n", step.model.Artifact)
			return NewEmittableError(err, "Downloading failed")
		}
	}

	downloadSize := "unknown"

	if f, ok := downloadedFile.(*cacheddownloader.CachedFile); ok {
		fi, err := f.Stat()
		if err != nil {
			return NewEmittableError(err, "Unable to obtain download size")
		}
		downloadSize = bytefmt.ByteSize(uint64(fi.Size()))
	}

	step.logger.Info("finished-download")
	step.emit("Downloaded %s (%s)\n", step.model.Artifact, downloadSize)

	streamInCompleteCh := make(chan struct{})

	go func() {
		select {
		case <-streamInCompleteCh:
		case <-step.cancelChan:
			downloadedFile.Close()
		}
	}()

	err = step.streamIn(step.model.To, downloadedFile)
	if err != nil {
		select {
		case <-step.cancelChan:
			return CancelError{}
		default:
			downloadedFile.Close()
		}
	}
	close(streamInCompleteCh)

	return err
}

func (step *downloadStep) download() (io.ReadCloser, error) {
	url, err := url.ParseRequestURI(step.model.From)
	if err != nil {
		step.logger.Error("parse-request-uri-error", err)
		return nil, err
	}

	tarStream, err := step.cachedDownloader.Fetch(url, step.model.CacheKey, cacheddownloader.TarTransform, step.cancelChan)
	if err != nil {
		step.logger.Error("failed-to-fetch", err)
		return nil, err
	}

	return tarStream, nil
}

func (step *downloadStep) streamIn(destination string, reader io.Reader) error {
	err := step.container.StreamIn(destination, reader)
	if err != nil {
		step.logger.Error("failed-to-stream-in", err, lager.Data{
			"destination": destination,
		})
		return NewEmittableError(err, "Copying into the container failed")
	}

	return err
}

func (step *downloadStep) emit(format string, a ...interface{}) {
	if step.model.Artifact != "" {
		fmt.Fprintf(step.streamer.Stdout(), format, a...)
	}
}
