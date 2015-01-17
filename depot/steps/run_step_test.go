package steps_test

import (
	"errors"
	"time"

	"github.com/pivotal-golang/lager/lagertest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"

	"github.com/cloudfoundry-incubator/garden"
	"github.com/cloudfoundry-incubator/garden/client/fake_api_client"
	gfakes "github.com/cloudfoundry-incubator/garden/fakes"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/gunk/timeprovider/faketimeprovider"

	"github.com/cloudfoundry-incubator/executor"
	"github.com/cloudfoundry-incubator/executor/depot/log_streamer/fake_log_streamer"
	. "github.com/cloudfoundry-incubator/executor/depot/steps"
)

var _ = Describe("RunAction", func() {
	var step Step

	var runAction models.RunAction
	var fakeStreamer *fake_log_streamer.FakeLogStreamer
	var gardenClient *fake_api_client.FakeClient
	var logger *lagertest.TestLogger
	var fileDescriptorLimit uint64
	var allowPrivileged bool
	var externalIP string
	var portMappings []executor.PortMapping
	var exportNetworkEnvVars bool
	var fakeTimeProvider *faketimeprovider.FakeTimeProvider

	var spawnedProcess *gfakes.FakeProcess
	var runError error

	BeforeEach(func() {
		fileDescriptorLimit = 17

		runAction = models.RunAction{
			Path: "sudo",
			Args: []string{"reboot"},
			Dir:  "/some-dir",
			Env: []models.EnvironmentVariable{
				{Name: "A", Value: "1"},
				{Name: "B", Value: "2"},
			},
			ResourceLimits: models.ResourceLimits{
				Nofile: &fileDescriptorLimit,
			},
			Privileged: false,
		}

		fakeStreamer = new(fake_log_streamer.FakeLogStreamer)
		fakeStreamer.StdoutReturns(noOpWriter{})
		gardenClient = fake_api_client.New()

		logger = lagertest.NewTestLogger("test")

		allowPrivileged = false

		spawnedProcess = new(gfakes.FakeProcess)
		runError = nil

		gardenClient.Connection.RunStub = func(string, garden.ProcessSpec, garden.ProcessIO) (garden.Process, error) {
			return spawnedProcess, runError
		}

		externalIP = "external-ip"
		portMappings = nil
		exportNetworkEnvVars = false
		fakeTimeProvider = faketimeprovider.New(time.Unix(123, 456))
	})

	handle := "some-container-handle"

	JustBeforeEach(func() {
		gardenClient.Connection.CreateReturns(handle, nil)

		container, err := gardenClient.Create(garden.ContainerSpec{})
		Ω(err).ShouldNot(HaveOccurred())

		step = NewRun(
			container,
			runAction,
			fakeStreamer,
			logger,
			allowPrivileged,
			externalIP,
			portMappings,
			exportNetworkEnvVars,
			fakeTimeProvider,
		)
	})

	Describe("Perform", func() {
		var stepErr error

		JustBeforeEach(func() {
			stepErr = step.Perform()
		})

		Context("with a privileged action", func() {
			BeforeEach(func() {
				runAction.Privileged = true
			})

			Context("with allowPrivileged set to false", func() {
				BeforeEach(func() {
					allowPrivileged = false
				})

				It("errors when trying to execute a privileged run action", func() {
					Ω(stepErr).Should(HaveOccurred())
				})

				It("logs the step", func() {
					Ω(logger.TestSink.LogMessages()).Should(ConsistOf([]string{
						"test.run-step.running",
						"test.run-step.privileged-action-denied",
					}))
				})
			})

			Context("with allowPrivileged set to true", func() {
				BeforeEach(func() {
					allowPrivileged = true
				})

				It("does not error when trying to execute a privileged run action", func() {
					Ω(stepErr).ShouldNot(HaveOccurred())
				})

				It("creates a privileged container", func() {
					_, spec, _ := gardenClient.Connection.RunArgsForCall(0)
					Ω(spec.Privileged).Should(BeTrue())
				})
			})
		})

		Context("when the script succeeds", func() {
			BeforeEach(func() {
				spawnedProcess.WaitReturns(0, nil)
			})

			It("does not return an error", func() {
				Ω(stepErr).ShouldNot(HaveOccurred())
			})

			It("executes the command in the passed-in container", func() {
				ranHandle, spec, _ := gardenClient.Connection.RunArgsForCall(0)
				Ω(ranHandle).Should(Equal(handle))
				Ω(spec.Path).Should(Equal("sudo"))
				Ω(spec.Args).Should(Equal([]string{"reboot"}))
				Ω(spec.Dir).Should(Equal("/some-dir"))
				Ω(*spec.Limits.Nofile).Should(BeNumerically("==", fileDescriptorLimit))
				Ω(spec.Env).Should(ContainElement("A=1"))
				Ω(spec.Env).Should(ContainElement("B=2"))
				Ω(spec.Privileged).Should(BeFalse())
			})

			It("logs the step", func() {
				Ω(logger.TestSink.LogMessages()).Should(ConsistOf([]string{
					"test.run-step.running",
					"test.run-step.creating-process",
					"test.run-step.successful-process-create",
					"test.run-step.process-exit",
				}))
			})
		})

		Context("when the script fails", func() {
			var waitErr error

			BeforeEach(func() {
				waitErr = errors.New("wait-error")
				spawnedProcess.WaitReturns(0, waitErr)
			})

			It("returns an error", func() {
				Ω(stepErr).Should(MatchError(waitErr))
			})

			It("logs the step", func() {
				Ω(logger.TestSink.LogMessages()).Should(ConsistOf([]string{
					"test.run-step.running",
					"test.run-step.creating-process",
					"test.run-step.successful-process-create",
					"test.run-step.running-error",
				}))
			})
		})

		Context("CF_INSTANCE_* networking env vars", func() {
			Context("when exportNetworkEnvVars is set to true", func() {
				BeforeEach(func() {
					exportNetworkEnvVars = true
				})

				It("sets CF_INSTANCE_IP on the container", func() {
					_, spec, _ := gardenClient.Connection.RunArgsForCall(0)
					Ω(spec.Env).Should(ContainElement("CF_INSTANCE_IP=external-ip"))
				})

				Context("when the container has port mappings configured", func() {
					BeforeEach(func() {
						portMappings = []executor.PortMapping{
							{HostPort: 1, ContainerPort: 2},
							{HostPort: 3, ContainerPort: 4},
						}
					})

					It("sets CF_INSTANCE_* networking env vars", func() {
						_, spec, _ := gardenClient.Connection.RunArgsForCall(0)
						Ω(spec.Env).Should(ContainElement("CF_INSTANCE_PORT=1"))
						Ω(spec.Env).Should(ContainElement("CF_INSTANCE_ADDR=external-ip:1"))
						Ω(spec.Env).Should(ContainElement("CF_INSTANCE_PORTS=1:2,3:4"))
					})
				})

				Context("when the container does not have any port mappings configured", func() {
					BeforeEach(func() {
						portMappings = []executor.PortMapping{}
					})

					It("sets all port-related env vars to the empty string", func() {
						_, spec, _ := gardenClient.Connection.RunArgsForCall(0)
						Ω(spec.Env).Should(ContainElement("CF_INSTANCE_PORT="))
						Ω(spec.Env).Should(ContainElement("CF_INSTANCE_ADDR="))
						Ω(spec.Env).Should(ContainElement("CF_INSTANCE_PORTS="))
					})
				})
			})

			Context("when exportNetworkEnvVars is set to false", func() {
				BeforeEach(func() {
					exportNetworkEnvVars = false
				})

				It("does not set CF_INSTANCE_IP on the container", func() {
					_, spec, _ := gardenClient.Connection.RunArgsForCall(0)
					Ω(spec.Env).ShouldNot(ContainElement("CF_INSTANCE_IP=external-ip"))
				})
			})
		})

		Context("when a file descriptor limit is not configured", func() {
			BeforeEach(func() {
				runAction.ResourceLimits.Nofile = nil
				spawnedProcess.WaitReturns(0, nil)
			})

			It("does not enforce it on the process", func() {
				_, spec, _ := gardenClient.Connection.RunArgsForCall(0)
				Ω(spec.Limits.Nofile).Should(BeNil())
			})
		})

		Context("when the script has a non-zero exit code", func() {
			BeforeEach(func() {
				spawnedProcess.WaitReturns(19, nil)
			})

			It("should return an emittable error with the exit code", func() {
				Ω(stepErr).Should(MatchError(NewEmittableError(nil, "Exited with status 19")))
			})
		})

		Context("when Garden errors", func() {
			disaster := errors.New("I, like, tried but failed")

			BeforeEach(func() {
				runError = disaster
			})

			It("returns the error", func() {
				Ω(stepErr).Should(Equal(disaster))
			})

			It("logs the step", func() {
				Ω(logger.TestSink.LogMessages()).Should(ConsistOf([]string{
					"test.run-step.running",
					"test.run-step.creating-process",
					"test.run-step.failed-creating-process",
				}))
			})
		})

		Context("regardless of status code, when an out of memory event has occured", func() {
			BeforeEach(func() {
				gardenClient.Connection.InfoReturns(
					garden.ContainerInfo{
						Events: []string{"happy land", "out of memory", "another event"},
					},
					nil,
				)

				spawnedProcess.WaitReturns(19, nil)
			})

			It("returns an emittable error", func() {
				Ω(stepErr).Should(MatchError(NewEmittableError(nil, "Exited with status 19 (out of memory)")))
			})
		})

		Context("when container info cannot be retrieved", func() {
			BeforeEach(func() {
				gardenClient.Connection.InfoReturns(garden.ContainerInfo{}, errors.New("info-error"))
				spawnedProcess.WaitReturns(19, nil)
			})

			It("logs the step", func() {
				Ω(logger.TestSink.LogMessages()).Should(ConsistOf([]string{
					"test.run-step.running",
					"test.run-step.creating-process",
					"test.run-step.successful-process-create",
					"test.run-step.process-exit",
					"test.run-step.failed-to-get-info",
				}))
			})
		})

		Describe("emitting logs", func() {
			var stdoutBuffer, stderrBuffer *gbytes.Buffer

			BeforeEach(func() {
				stdoutBuffer = gbytes.NewBuffer()
				stderrBuffer = gbytes.NewBuffer()

				fakeStreamer.StdoutReturns(stdoutBuffer)
				fakeStreamer.StderrReturns(stderrBuffer)

				spawnedProcess.WaitStub = func() (int, error) {
					_, _, io := gardenClient.Connection.RunArgsForCall(0)

					_, err := io.Stdout.Write([]byte("hi out"))
					Ω(err).ShouldNot(HaveOccurred())

					_, err = io.Stderr.Write([]byte("hi err"))
					Ω(err).ShouldNot(HaveOccurred())

					return 34, nil
				}
			})

			It("emits the output chunks as they come in", func() {
				Ω(stdoutBuffer).Should(gbytes.Say("hi out"))
				Ω(stderrBuffer).Should(gbytes.Say("hi err"))
			})

			It("should flush the output when the code exits", func() {
				Ω(fakeStreamer.FlushCallCount()).Should(Equal(1))
			})

			It("emits the exit status code", func() {
				Ω(stdoutBuffer).Should(gbytes.Say("Exit status 34"))
			})
		})
	})

	Describe("Cancel", func() {
		var (
			performErr chan error

			waiting    chan struct{}
			waitExited chan int
		)

		BeforeEach(func() {
			performErr = make(chan error)

			waiting = make(chan struct{})
			waitExited = make(chan int, 1)

			spawnedProcess.WaitStub = func() (int, error) {
				close(waiting)
				return <-waitExited, nil
			}
		})

		JustBeforeEach(func() {
			go func() {
				performErr <- step.Perform()
				close(performErr)
			}()

			Eventually(waiting).Should(BeClosed())
			step.Cancel()
		})

		AfterEach(func() {
			close(waitExited)
			Eventually(performErr).Should(BeClosed())
		})

		It("sends an interrupt to the process", func() {
			Eventually(spawnedProcess.SignalCallCount).Should(Equal(1))
			Ω(spawnedProcess.SignalArgsForCall(0)).Should(Equal(garden.SignalTerminate))
		})

		Context("when the process exits", func() {
			It("completes the perform without having sent kill", func() {
				waitExited <- (128 + 15)

				Eventually(performErr).Should(Receive(Equal(ErrCancelled)))

				Ω(spawnedProcess.SignalCallCount()).Should(Equal(1))
			})
		})

		Context("when the process does not exit after 10s", func() {
			It("sends a kill signal to the process", func() {
				Eventually(spawnedProcess.SignalCallCount).Should(Equal(1))

				fakeTimeProvider.Increment(TERMINATE_TIMEOUT + 1*time.Second)

				Eventually(spawnedProcess.SignalCallCount).Should(Equal(2))
				Ω(spawnedProcess.SignalArgsForCall(1)).Should(Equal(garden.SignalKill))

				waitExited <- (128 + 9)

				Eventually(performErr).Should(Receive(Equal(ErrCancelled)))
			})

			Context("when the process *still* does not exit after 1m", func() {
				It("finishes performing with failure", func() {
					Eventually(spawnedProcess.SignalCallCount).Should(Equal(1))

					fakeTimeProvider.Increment(TERMINATE_TIMEOUT + 1*time.Second)

					Eventually(spawnedProcess.SignalCallCount).Should(Equal(2))
					Ω(spawnedProcess.SignalArgsForCall(1)).Should(Equal(garden.SignalKill))

					fakeTimeProvider.Increment(TERMINATE_TIMEOUT + 1*time.Second)

					Consistently(performErr).ShouldNot(Receive())

					fakeTimeProvider.Increment(EXIT_TIMEOUT + 1*time.Second)

					Eventually(performErr).Should(Receive(Equal(ErrExitTimeout)))

					Ω(logger.TestSink.LogMessages()).Should(ContainElement(
						ContainSubstring("process-did-not-exit"),
					))
				})
			})
		})
	})
})

type noOpWriter struct{}

func (w noOpWriter) Write(b []byte) (int, error) { return len(b), nil }
