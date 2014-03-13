package action_runner_test

import (
	"errors"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	. "github.com/cloudfoundry-incubator/executor/action_runner"
	"github.com/cloudfoundry-incubator/executor/action_runner/fake_action"
)

var _ = Describe("ActionRunner", func() {
	It("performs them all in order and sends back nil", func(done Done) {
		defer close(done)

		seq := make(chan int, 3)

		runner := New([]Action{
			fake_action.FakeAction{
				WhenPerforming: func() error {
					seq <- 1
					return nil
				},
			},
			fake_action.FakeAction{
				WhenPerforming: func() error {
					seq <- 2
					return nil
				},
			},
			fake_action.FakeAction{
				WhenPerforming: func() error {
					seq <- 3
					return nil
				},
			},
		})

		result := make(chan error)
		go func() { result <- runner.Perform() }()

		Ω(<-seq).Should(Equal(1))
		Ω(<-seq).Should(Equal(2))
		Ω(<-seq).Should(Equal(3))

		Ω(<-result).Should(BeNil())
	})

	It("cleans up the actions in reverse order before sending back the result", func(done Done) {
		defer close(done)

		cleanup := make(chan int, 3)

		runner := New([]Action{
			fake_action.FakeAction{
				WhenCleaningUp: func() {
					cleanup <- 1
				},
			},
			fake_action.FakeAction{
				WhenCleaningUp: func() {
					cleanup <- 2
				},
			},
			fake_action.FakeAction{
				WhenCleaningUp: func() {
					cleanup <- 3
				},
			},
		})

		result := make(chan error)
		go func() { result <- runner.Perform() }()

		Ω(<-cleanup).Should(Equal(3))
		Ω(<-cleanup).Should(Equal(2))
		Ω(<-cleanup).Should(Equal(1))

		Ω(<-result).Should(BeNil())
	})

	Context("when an action fails in the middle", func() {
		It("sends back the error and does not continue performing, and cleans up completed actions", func(done Done) {
			defer close(done)

			disaster := errors.New("oh no!")

			seq := make(chan int, 3)
			cleanup := make(chan int, 3)

			runner := New([]Action{
				fake_action.FakeAction{
					WhenPerforming: func() error {
						seq <- 1
						return nil
					},
					WhenCleaningUp: func() {
						cleanup <- 1
					},
				},
				fake_action.FakeAction{
					WhenPerforming: func() error {
						return disaster
					},
					WhenCleaningUp: func() {
						cleanup <- 2
					},
				},
				fake_action.FakeAction{
					WhenPerforming: func() error {
						seq <- 3
						return nil
					},
					WhenCleaningUp: func() {
						cleanup <- 3
					},
				},
			})

			result := make(chan error)
			go func() { result <- runner.Perform() }()

			Ω(<-seq).Should(Equal(1))
			Ω(<-cleanup).Should(Equal(1))

			Ω(<-result).Should(Equal(disaster))

			Consistently(seq).ShouldNot(Receive())
			Consistently(cleanup).ShouldNot(Receive())
		})
	})

	Context("when the runner is canceled in the middle", func() {
		It("cancels the running action and waits for completed actions to be cleaned up", func(done Done) {
			defer close(done)

			seq := make(chan int, 3)
			cleanup := make(chan int, 3)

			waitingForInterrupt := make(chan bool)
			interrupt := make(chan bool)
			interrupted := make(chan bool)

			startCleanup := make(chan bool)

			runner := New([]Action{
				fake_action.FakeAction{
					WhenPerforming: func() error {
						seq <- 1
						return nil
					},
					WhenCleaningUp: func() {
						<-startCleanup
						cleanup <- 1
					},
				},
				fake_action.FakeAction{
					WhenPerforming: func() error {
						seq <- 2

						waitingForInterrupt <- true
						<-interrupt
						interrupted <- true

						return nil
					},
					WhenCancelling: func() {
						interrupt <- true
					},
					WhenCleaningUp: func() {
						cleanup <- 2
					},
				},
				fake_action.FakeAction{
					WhenPerforming: func() error {
						seq <- 3
						return nil
					},
					WhenCleaningUp: func() {
						cleanup <- 3
					},
				},
			})

			result := make(chan error)
			go func() { result <- runner.Perform() }()

			Ω(<-seq).Should(Equal(1))
			Ω(<-seq).Should(Equal(2))

			<-waitingForInterrupt

			runner.Cancel()

			<-interrupted

			startCleanup <- true

			Ω(<-cleanup).Should(Equal(1))

			Ω(<-result).Should(Equal(CancelledError))

			Consistently(seq).ShouldNot(Receive())
			Consistently(cleanup).ShouldNot(Receive())
		})
	})
})
