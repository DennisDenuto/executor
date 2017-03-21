package initializer_test

import (
	"encoding/asn1"
	"net/http"
	"os"
	"time"

	"code.cloudfoundry.org/clock/fakeclock"
	"code.cloudfoundry.org/durationjson"
	"code.cloudfoundry.org/executor"
	"code.cloudfoundry.org/executor/depot/containerstore"
	"code.cloudfoundry.org/executor/initializer"
	"code.cloudfoundry.org/executor/initializer/configuration"
	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"
	fake_metric "github.com/cloudfoundry/dropsonde/metric_sender/fake"
	"github.com/cloudfoundry/dropsonde/metrics"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
)

var _ = Describe("Initializer", func() {
	var initialTime time.Time
	var sender *fake_metric.FakeMetricSender
	var fakeGarden *ghttp.Server
	var fakeClock *fakeclock.FakeClock
	var errCh chan error
	var done chan struct{}
	var config initializer.ExecutorConfig
	var logger lager.Logger

	BeforeEach(func() {
		initialTime = time.Now()
		sender = fake_metric.NewFakeMetricSender()
		metrics.Initialize(sender, nil)
		fakeGarden = ghttp.NewUnstartedServer()
		fakeClock = fakeclock.NewFakeClock(initialTime)
		errCh = make(chan error, 1)
		done = make(chan struct{})
		logger = lagertest.NewTestLogger("test")

		fakeGarden.RouteToHandler("GET", "/ping", ghttp.RespondWithJSONEncoded(http.StatusOK, struct{}{}))
		fakeGarden.RouteToHandler("GET", "/containers", ghttp.RespondWithJSONEncoded(http.StatusOK, struct{}{}))
		fakeGarden.RouteToHandler("GET", "/capacity", ghttp.RespondWithJSONEncoded(http.StatusOK,
			garden.Capacity{MemoryInBytes: 1024 * 1024 * 1024, DiskInBytes: 20 * 1048 * 1024 * 1024, MaxContainers: 4}))
		fakeGarden.RouteToHandler("GET", "/containers/bulk_info", ghttp.RespondWithJSONEncoded(http.StatusOK, struct{}{}))
		config = initializer.ExecutorConfig{
			AutoDiskOverheadMB:                 1,
			CachePath:                          "/tmp/cache",
			ContainerInodeLimit:                200000,
			ContainerMaxCpuShares:              0,
			ContainerMetricsReportInterval:     durationjson.Duration(15 * time.Second),
			ContainerOwnerName:                 "executor",
			ContainerReapInterval:              durationjson.Duration(time.Minute),
			CreateWorkPoolSize:                 32,
			DeleteWorkPoolSize:                 32,
			DiskMB:                             configuration.Automatic,
			ExportNetworkEnvVars:               false,
			GardenAddr:                         "/tmp/garden.sock",
			GardenHealthcheckCommandRetryPause: durationjson.Duration(1 * time.Second),
			GardenHealthcheckEmissionInterval:  durationjson.Duration(30 * time.Second),
			GardenHealthcheckInterval:          durationjson.Duration(10 * time.Minute),
			GardenHealthcheckProcessArgs:       []string{},
			GardenHealthcheckProcessEnv:        []string{},
			GardenHealthcheckTimeout:           durationjson.Duration(10 * time.Minute),
			GardenNetwork:                      "unix",
			HealthCheckContainerOwnerName:      "executor-health-check",
			HealthCheckWorkPoolSize:            64,
			HealthyMonitoringInterval:          durationjson.Duration(30 * time.Second),
			MaxCacheSizeInBytes:                10 * 1024 * 1024 * 1024,
			MaxConcurrentDownloads:             5,
			MemoryMB:                           configuration.Automatic,
			MetricsWorkPoolSize:                8,
			ReadWorkPoolSize:                   64,
			ReservedExpirationTime:             durationjson.Duration(time.Minute),
			SkipCertVerify:                     false,
			TempDir:                            "/tmp",
			UnhealthyMonitoringInterval:        durationjson.Duration(500 * time.Millisecond),
			VolmanDriverPaths:                  "/tmpvolman1:/tmp/volman2",
		}
	})

	AfterEach(func() {
		Eventually(done).Should(BeClosed())
		fakeGarden.Close()
	})

	JustBeforeEach(func() {
		fakeGarden.Start()
		config.GardenAddr = fakeGarden.HTTPTestServer.Listener.Addr().String()
		config.GardenNetwork = "tcp"
		go func() {
			_, _, err := initializer.Initialize(logger, config, "fake-rootfs", fakeClock)
			errCh <- err
			close(done)
		}()
	})

	checkStalledMetric := func() float64 {
		return sender.GetValue("StalledGardenDuration").Value
	}

	Context("when garden doesn't respond", func() {
		var waitChan chan struct{}

		BeforeEach(func() {
			waitChan = make(chan struct{})
			fakeGarden.RouteToHandler("GET", "/ping", func(w http.ResponseWriter, req *http.Request) {
				<-waitChan
				ghttp.RespondWithJSONEncoded(http.StatusOK, struct{}{})(w, req)
			})
		})

		AfterEach(func() {
			close(waitChan)
		})

		It("emits metrics when garden doesn't respond", func() {
			Consistently(checkStalledMetric, 10*time.Millisecond).Should(BeEquivalentTo(0))
			fakeClock.WaitForWatcherAndIncrement(initializer.StalledMetricHeartbeatInterval)
			Eventually(checkStalledMetric).Should(BeNumerically("~", fakeClock.Since(initialTime)))
		})
	})

	Context("when garden responds", func() {
		It("emits 0", func() {
			Eventually(func() bool { return sender.HasValue("StalledGardenDuration") }).Should(BeTrue())
			Expect(checkStalledMetric()).To(BeEquivalentTo(0))
			Consistently(errCh).ShouldNot(Receive(HaveOccurred()))
		})
	})

	Context("when garden responds with an error", func() {
		var retried chan struct{}

		BeforeEach(func() {
			callCount := 0
			retried = make(chan struct{})
			fakeGarden.RouteToHandler("GET", "/ping", func(w http.ResponseWriter, req *http.Request) {
				callCount++
				if callCount == 1 {
					ghttp.RespondWith(http.StatusInternalServerError, "")(w, req)
				} else if callCount == 2 {
					ghttp.RespondWithJSONEncoded(http.StatusOK, struct{}{})(w, req)
					close(retried)
				}
			})
		})

		It("retries on a timer until it succeeds", func() {
			Consistently(retried).ShouldNot(BeClosed())
			fakeClock.Increment(initializer.PingGardenInterval)
			Eventually(retried).Should(BeClosed())
		})

		It("emits zero once it succeeds", func() {
			Consistently(func() bool { return sender.HasValue("StalledGardenDuration") }).Should(BeFalse())
			fakeClock.Increment(initializer.PingGardenInterval)
			Eventually(func() bool { return sender.HasValue("StalledGardenDuration") }).Should(BeTrue())
			Expect(checkStalledMetric()).To(BeEquivalentTo(0))
		})

		Context("when the error is unrecoverable", func() {
			BeforeEach(func() {
				fakeGarden.RouteToHandler(
					"GET",
					"/ping",
					ghttp.RespondWith(http.StatusGatewayTimeout, `{ "Type": "UnrecoverableError" , "Message": "Extra Special Error Message"}`),
				)
			})

			It("returns an error", func() {
				Eventually(errCh).Should(Receive(BeAssignableToTypeOf(garden.UnrecoverableError{})))
			})
		})
	})

	Context("when the post setup hook is invalid", func() {
		BeforeEach(func() {
			config.PostSetupHook = "unescaped quote\\"
		})

		It("fails fast", func() {
			Eventually(errCh).Should(Receive(MatchError("EOF found after escape character")))
		})
	})

	Describe("with the TLS configuration", func() {
		Context("when the TLS config is valid", func() {
			BeforeEach(func() {
				config.PathToTLSCert = "fixtures/downloader/client.crt"
				config.PathToTLSKey = "fixtures/downloader/client.key"
				config.PathToTLSCACert = "fixtures/downloader/ca.crt"
			})

			It("uses the certs for the uploader and cacheddownloader", func() {
				// not really an easy way to check this at this layer -- inigo
				// let's just check that our validation passes
				Consistently(errCh).ShouldNot(Receive(HaveOccurred()))
			})

			Context("when no CA cert is provided", func() {
				BeforeEach(func() {
					config.PathToTLSCACert = ""
				})

				It("still passes validation", func() {
					Consistently(errCh).ShouldNot(Receive(HaveOccurred()))
				})
			})

			Context("when a CA cert is provided, but no keypair", func() {
				BeforeEach(func() {
					config.PathToTLSCert = ""
					config.PathToTLSKey = ""
				})

				It("passes still passes validation", func() {
					Consistently(errCh).ShouldNot(Receive(HaveOccurred()))
				})
			})
		})

		Context("when the certs are invalid", func() {
			BeforeEach(func() {
				config.PathToTLSCert = "fixtures/ca-certs-invalid"
				config.PathToTLSKey = "fixtures/downloader/client.key"
				config.PathToTLSCACert = "fixtures/downloader/ca.crt"
			})

			It("fails", func() {
				Eventually(errCh).Should(Receive(MatchError(ContainSubstring("failed to find any PEM data in certificate input"))))
			})

			Context("when the cert is missing", func() {
				BeforeEach(func() {
					config.PathToTLSCert = ""
				})

				It("fails", func() {
					Eventually(errCh).Should(Receive(MatchError(ContainSubstring("The TLS certificate or key is missing"))))
				})
			})

			Context("when the key is missing", func() {
				BeforeEach(func() {
					config.PathToTLSKey = ""
				})

				It("fails", func() {
					Eventually(errCh).Should(Receive(MatchError(ContainSubstring("The TLS certificate or key is missing"))))
				})
			})
		})

		Context("when the TLS properties are missing", func() {
			It("succeeds", func() {
				// not really an easy way to check this at this layer -- inigo
				// let's just check that our validation passes
				Consistently(errCh).ShouldNot(Receive(HaveOccurred()))
			})
		})
	})

	Describe("configuring trusted CA bundle", func() {
		Context("when valid", func() {
			BeforeEach(func() {
				config.PathToCACertsForDownloads = "fixtures/ca-certs"
			})

			It("uses it for the cached downloader", func() {
				// not really an easy way to check this at this layer -- inigo
				// let's just check that our validation passes
				Consistently(errCh).ShouldNot(Receive(HaveOccurred()))
			})

			Context("when the cert bundle has extra leading and trailing spaces", func() {
				BeforeEach(func() {
					config.PathToCACertsForDownloads = "fixtures/ca-certs-with-spaces"
				})

				It("does not error", func() {
					Consistently(errCh).ShouldNot(Receive(HaveOccurred()))
				})
			})

			Context("when the cert bundle is empty", func() {
				BeforeEach(func() {
					config.PathToCACertsForDownloads = "fixtures/ca-certs-empty"
				})

				It("does not error", func() {
					Consistently(errCh).ShouldNot(Receive(HaveOccurred()))
				})
			})
		})

		Context("when certs are invalid", func() {
			BeforeEach(func() {
				config.PathToCACertsForDownloads = "fixtures/ca-certs-invalid"
			})

			It("fails", func() {
				Eventually(errCh).Should(Receive(MatchError("unable to load CA certificate")))
			})
		})

		Context("when path is invalid", func() {
			BeforeEach(func() {
				config.PathToCACertsForDownloads = "sandwich"
			})

			It("fails", func() {
				Eventually(errCh).Should(Receive(MatchError("Unable to open CA cert bundle 'sandwich'")))
			})
		})
	})

	Describe("CredManagerFromConfig", func() {
		var credManager containerstore.CredManager
		var err error
		var container executor.Container
		var logger *lagertest.TestLogger

		JustBeforeEach(func() {
			logger = lagertest.NewTestLogger("executor")
			container = executor.Container{
				Guid: "1234",
			}
			credManager, err = initializer.CredManagerFromConfig(logger, config, fakeClock)
		})

		Describe("when instance identity creds directory is not set", func() {
			BeforeEach(func() {
				config.InstanceIdentityCredDir = ""
			})

			It("returns a noop credential manager", func() {
				bindMounts, _, err := credManager.CreateCredDir(logger, container)
				Expect(bindMounts).To(BeEmpty())
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Describe("when the instance identity creds directory is set", func() {
			BeforeEach(func() {
				config.InstanceIdentityCredDir = "fixtures/instance-id/"
				config.InstanceIdentityCAPath = "fixtures/instance-id/ca.crt"
				config.InstanceIdentityPrivateKeyPath = "fixtures/instance-id/ca.key"
				config.InstanceIdentityValidityPeriod = durationjson.Duration(1 * time.Minute)
			})

			It("returns a credential manager", func() {
				bindMounts, _, err := credManager.CreateCredDir(logger, container)
				defer credManager.RemoveCreds(logger, container)
				Expect(err).NotTo(HaveOccurred())
				Expect(bindMounts).NotTo(BeEmpty())
			})

			Context("when the private key does not exist", func() {
				BeforeEach(func() {
					config.InstanceIdentityPrivateKeyPath = "fixtures/instance-id/notexist.key"
				})

				It("fails", func() {
					Eventually(os.IsNotExist(err)).Should(BeTrue(), "Private key does not exist")
				})
			})

			Context("when the private key is not PEM-encoded", func() {
				BeforeEach(func() {
					config.InstanceIdentityPrivateKeyPath = "fixtures/instance-id/non-pem.key"
				})

				It("fails", func() {
					Eventually(err).Should(MatchError(ContainSubstring("instance ID key is not PEM-encoded")))
				})
			})

			Context("when the private key is invalid", func() {
				BeforeEach(func() {
					config.InstanceIdentityPrivateKeyPath = "fixtures/instance-id/invalid.key"
				})

				It("fails", func() {
					Eventually(err).Should(BeAssignableToTypeOf(asn1.StructuralError{}))
				})
			})

			Context("when the certificate does not exist", func() {
				BeforeEach(func() {
					config.InstanceIdentityCAPath = "fixtures/instance-id/notexist.crt"
				})

				It("fails", func() {
					Eventually(os.IsNotExist(err)).Should(BeTrue(), "Instance certificate does not exist")
				})
			})

			Context("when the certificate is not PEM-encoded", func() {
				BeforeEach(func() {
					config.InstanceIdentityCAPath = "fixtures/instance-id/non-pem.crt"
				})

				It("fails", func() {
					Eventually(err).Should(MatchError(ContainSubstring("instance ID CA is not PEM-encoded")))
				})
			})

			Context("when the certificate is invalid", func() {
				BeforeEach(func() {
					config.InstanceIdentityCAPath = "fixtures/instance-id/invalid.crt"
				})

				It("fails", func() {
					Eventually(err).Should(BeAssignableToTypeOf(asn1.StructuralError{}))
				})
			})

			Context("when the validity period is not set", func() {
				BeforeEach(func() {
					config.InstanceIdentityValidityPeriod = 0
				})

				It("fails", func() {
					Eventually(err).Should(MatchError(ContainSubstring("instance ID validity period needs to be set and positive")))
				})
			})
		})
	})
})
