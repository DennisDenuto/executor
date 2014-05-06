package executor_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"

	steno "github.com/cloudfoundry/gosteno"
	"github.com/pivotal-golang/archiver/compressor/fake_compressor"
	"github.com/pivotal-golang/archiver/extractor/fake_extractor"
	"github.com/pivotal-golang/cacheddownloader/fakecacheddownloader"
	"github.com/tedsuo/router"

	. "github.com/cloudfoundry-incubator/executor/executor"
	"github.com/cloudfoundry-incubator/executor/log_streamer_factory"
	"github.com/cloudfoundry-incubator/executor/transformer"
	"github.com/cloudfoundry-incubator/executor/uploader/fake_uploader"
	"github.com/cloudfoundry-incubator/garden/client/fake_warden_client"
	"github.com/cloudfoundry-incubator/runtime-schema/models/executor_api"
	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/config"
	. "github.com/onsi/gomega"
)

var _ = Describe("Executor", func() {
	var (
		executor     *Executor
		wardenClient *fake_warden_client.FakeClient
		logger       *steno.Logger
		trans        *transformer.Transformer
		executorURL  string
		reqGen       *router.RequestGenerator
	)

	BeforeEach(func() {
		steno.EnterTestMode()
		logger = steno.NewLogger("test-logger")
		wardenClient = fake_warden_client.New()
		trans = transformer.NewTransformer(
			log_streamer_factory.New("", ""),
			fakecacheddownloader.New(),
			fake_uploader.New(),
			&fake_extractor.FakeExtractor{},
			&fake_compressor.FakeCompressor{},
			logger,
			"/tmp",
		)
		executorURL = fmt.Sprintf("127.0.0.1:%d", 5001+config.GinkgoConfig.ParallelNode)

		reqGen = router.NewRequestGenerator("http://"+executorURL, executor_api.Routes)

		executor = New(executorURL, 100, 1024, 1024, wardenClient, trans, time.Second, logger)
	})

	Describe("Executor IDs", func() {
		It("should generate a random ID when created", func() {
			executor2 := New(executorURL, 100, 1024, 1024, wardenClient, trans, time.Second, logger)

			Ω(executor.ID()).ShouldNot(BeZero())
			Ω(executor2.ID()).ShouldNot(BeZero())

			Ω(executor.ID()).ShouldNot(Equal(executor2.ID()))
		})
	})

	Describe("Run", func() {
		var errChan chan error
		var sigChan chan os.Signal

		BeforeEach(func() {
			errChan = make(chan error)
			sigChan = make(chan os.Signal)
			ready := make(chan struct{})
			go func() {
				errChan <- executor.Run(sigChan, ready)
			}()
			Eventually(ready).Should(BeClosed())
		})

		Context("while running", func() {
			AfterEach(func() {
				sigChan <- syscall.SIGTERM
				Eventually(errChan).Should(Receive(BeNil()))
			})

			It("spins up an API server", func() {
				payload, err := json.Marshal(executor_api.ContainerAllocationRequest{
					MemoryMB: 32,
					DiskMB:   512,
				})
				Ω(err).ShouldNot(HaveOccurred())

				req, err := reqGen.RequestForHandler(executor_api.AllocateContainer, nil, bytes.NewBuffer(payload))
				Ω(err).ShouldNot(HaveOccurred())

				res, err := http.DefaultClient.Do(req)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(res.StatusCode).Should(Equal(http.StatusCreated))
			})
		})

		Context("after receiving SIGINT", func() {
			var err error
			BeforeEach(func() {
				sigChan <- syscall.SIGTERM
				err = <-errChan
			})

			It("completes without error", func() {
				Ω(err).Should(BeNil())
			})

			It("shuts down the API server", func() {
				req, err := reqGen.RequestForHandler(executor_api.GetContainer, router.Params{"guid": "123"}, nil)
				Ω(err).ShouldNot(HaveOccurred())

				_, err = http.DefaultClient.Do(req)
				Ω(err).Should(HaveOccurred())
			})
		})
	})
})
