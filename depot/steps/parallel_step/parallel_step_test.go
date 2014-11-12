package parallel_step_test

import (
	"errors"
	"sync"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/cloudfoundry-incubator/executor/depot/sequence"
	"github.com/cloudfoundry-incubator/executor/depot/sequence/fake_step"
	. "github.com/cloudfoundry-incubator/executor/depot/steps/parallel_step"
)

var _ = Describe("ParallelStep", func() {
	var step sequence.Step
	var subStep1 sequence.Step
	var subStep2 sequence.Step

	var thingHappened chan bool
	var cancelled chan bool

	BeforeEach(func() {
		thingHappened = make(chan bool, 2)
		cancelled = make(chan bool, 2)

		running := new(sync.WaitGroup)
		running.Add(2)

		subStep1 = &fake_step.FakeStep{
			PerformStub: func() error {
				running.Done()
				running.Wait()
				thingHappened <- true
				return nil
			},
			CancelStub: func() {
				cancelled <- true
			},
		}

		subStep2 = &fake_step.FakeStep{
			PerformStub: func() error {
				running.Done()
				running.Wait()
				thingHappened <- true
				return nil
			},
			CancelStub: func() {
				cancelled <- true
			},
		}
	})

	JustBeforeEach(func() {
		step = New([]sequence.Step{subStep1, subStep2})
	})

	It("performs its substeps in parallel", func(done Done) {
		defer close(done)

		err := step.Perform()
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(thingHappened).Should(Receive())
		Eventually(thingHappened).Should(Receive())
	}, 2)

	Context("when one of the substeps fails", func() {
		disaster := errors.New("oh no!")
		var triggerStep2 chan struct{}
		var step2Completed chan struct{}

		BeforeEach(func() {
			triggerStep2 = make(chan struct{})
			step2Completed = make(chan struct{})

			subStep1 = &fake_step.FakeStep{
				PerformStub: func() error {
					return disaster
				},
			}

			subStep2 = &fake_step.FakeStep{
				PerformStub: func() error {
					<-triggerStep2
					close(step2Completed)
					return nil
				},
			}
		})

		It("waits for the rest to finish", func() {
			errs := make(chan error)

			go func() {
				errs <- step.Perform()
			}()

			Consistently(errs).ShouldNot(Receive())

			close(triggerStep2)

			Eventually(step2Completed).Should(BeClosed())
			Eventually(errs).Should(Receive(Equal(disaster)))
		})
	})

	Context("when told to cancel", func() {
		It("passes the message along", func() {
			step.Cancel()

			Eventually(cancelled).Should(Receive())
			Eventually(cancelled).Should(Receive())
		})
	})
})
