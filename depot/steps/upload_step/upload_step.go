package upload_step

import (
	"archive/tar"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/executor/depot/log_streamer"
	"github.com/cloudfoundry-incubator/executor/depot/steps/emittable_error"
	"github.com/cloudfoundry-incubator/executor/depot/uploader"
	garden_api "github.com/cloudfoundry-incubator/garden/api"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/pivotal-golang/archiver/compressor"
	"github.com/pivotal-golang/bytefmt"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/semaphore"
)

type UploadStep struct {
	container  garden_api.Container
	model      models.UploadAction
	uploader   uploader.Uploader
	compressor compressor.Compressor
	tempDir    string
	semaphore  semaphore.Semaphore
	streamer   log_streamer.LogStreamer
	logger     lager.Logger
}

func New(
	container garden_api.Container,
	model models.UploadAction,
	uploader uploader.Uploader,
	compressor compressor.Compressor,
	tempDir string,
	semaphore semaphore.Semaphore,
	streamer log_streamer.LogStreamer,
	logger lager.Logger,
) *UploadStep {
	logger = logger.Session("UploadAction", lager.Data{
		"from": model.From,
	})

	return &UploadStep{
		container:  container,
		model:      model,
		uploader:   uploader,
		compressor: compressor,
		tempDir:    tempDir,
		streamer:   streamer,
		logger:     logger,
		semaphore:  semaphore,
	}
}

const (
	ErrCreateTmpDir    = "Failed to create temp dir"
	ErrEstablishStream = "Failed to establish stream from container"
	ErrReadTar         = "Failed to find first item in tar stream"
	ErrCreateTmpFile   = "Failed to create temp file"
	ErrCopyStreamToTmp = "Failed to copy stream contents into temp file"
)

func (step *UploadStep) Perform() (err error) {
	startWaiting := time.Now()
	resource, err := step.semaphore.Acquire()
	if err != nil {
		return err
	}

	step.logger.Info(fmt.Sprintf("upload-wait-time=%v", time.Since(startWaiting).Seconds()))
	startUploading := time.Now()

	defer func() {
		resource.Release()
		step.logger.Info(fmt.Sprintf("upload-time=%v", time.Since(startUploading).Seconds()))
	}()

	step.logger.Info("upload-starting")
	url, err := url.ParseRequestURI(step.model.To)
	if err != nil {
		// Do not emit error in case it leaks sensitive data in URL
		return err
	}

	tempDir, err := ioutil.TempDir(step.tempDir, "upload")
	if err != nil {
		return emittable_error.New(err, ErrCreateTmpDir)
	}

	defer os.RemoveAll(tempDir)

	outStream, err := step.container.StreamOut(step.model.From)
	if err != nil {
		return emittable_error.New(err, ErrEstablishStream)
	}
	defer outStream.Close()

	tarStream := tar.NewReader(outStream)
	_, err = tarStream.Next()
	if err != nil {
		return emittable_error.New(err, ErrReadTar)
	}

	tempFile, err := ioutil.TempFile(step.tempDir, "compressed")
	if err != nil {
		return emittable_error.New(err, ErrCreateTmpFile)
	}
	defer tempFile.Close()

	_, err = io.Copy(tempFile, tarStream)
	if err != nil {
		return emittable_error.New(err, ErrCopyStreamToTmp)
	}
	finalFileLocation := tempFile.Name()

	defer os.RemoveAll(finalFileLocation)

	uploadedBytes, err := step.uploader.Upload(finalFileLocation, url)
	if err != nil {
		// Do not emit error in case it leaks sensitive data in URL
		return err
	}

	fmt.Fprintf(step.streamer.Stdout(), fmt.Sprintf("Uploaded (%s)\n", bytefmt.ByteSize(uint64(uploadedBytes))))

	step.logger.Info("upload-successful")
	return nil
}

func (step *UploadStep) Cancel() {}

func (step *UploadStep) Cleanup() {}
