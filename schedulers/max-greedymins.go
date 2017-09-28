package schedulers

import (
	"bitbucket.org/sunybingcloud/elektron/def"
	"bitbucket.org/sunybingcloud/elektron/utilities/mesosUtils"
	"bitbucket.org/sunybingcloud/elektron/utilities/offerUtils"
	"fmt"
	"github.com/golang/protobuf/proto"
	mesos "github.com/mesos/mesos-go/mesosproto"
	"github.com/mesos/mesos-go/mesosutil"
	sched "github.com/mesos/mesos-go/scheduler"
	"log"
	"time"
)

// Decides if to take an offer or not.
func (s *MaxGreedyMins) takeOffer(offer *mesos.Offer, task def.Task,
	totalCPU, totalRAM, totalWatts float64) bool {

	cpus, mem, watts := offerUtils.OfferAgg(offer)

	//TODO: Insert watts calculation here instead of taking them as a parameter

	wattsConsideration, err := def.WattsToConsider(task, s.classMapWatts, offer)
	if err != nil {
		// Error in determining wattsConsideration.
		log.Fatal(err)
	}
	if (cpus >= (totalCPU + task.CPU)) && (mem >= (totalRAM + task.RAM)) &&
		(!s.wattsAsAResource || (watts >= (totalWatts + wattsConsideration))) {
		return true
	}
	return false
}

type MaxGreedyMins struct {
	base //Type embedding to inherit common functions.
}

// Initialization.
func (s *MaxGreedyMins) init(opts ...schedPolicyOption) {
	s.base.init(opts...)
}

func (s *MaxGreedyMins) newTask(offer *mesos.Offer, task def.Task) *mesos.TaskInfo {
	taskName := fmt.Sprintf("%s-%d", task.Name, *task.Instances)
	s.tasksCreated++

	// Start recording only when we're creating the first task.
	if !*s.RecordPCP {
		// Turn on logging
		*s.RecordPCP = true
		time.Sleep(1 * time.Second) // Make sure we're recording by the time the first task starts.
	}

	// If this is our first time running into this Agent.
	if _, ok := s.running[offer.GetSlaveId().GoString()]; !ok {
		s.running[offer.GetSlaveId().GoString()] = make(map[string]bool)
	}

	// Add task to list of tasks running on node.
	s.running[offer.GetSlaveId().GoString()][taskName] = true

	resources := []*mesos.Resource{
		mesosutil.NewScalarResource("cpus", task.CPU),
		mesosutil.NewScalarResource("mem", task.RAM),
	}

	if s.wattsAsAResource {
		if wattsToConsider, err := def.WattsToConsider(task, s.classMapWatts, offer); err == nil {
			log.Printf("Watts considered for host[%s] and task[%s] = %f", *offer.Hostname, task.Name, wattsToConsider)
			resources = append(resources, mesosutil.NewScalarResource("watts", wattsToConsider))
		} else {
			// Error in determining wattsConsideration.
			log.Fatal(err)
		}
	}

	return &mesos.TaskInfo{
		Name: proto.String(taskName),
		TaskId: &mesos.TaskID{
			Value: proto.String("elektron-" + taskName),
		},
		SlaveId:   offer.SlaveId,
		Resources: resources,
		Command: &mesos.CommandInfo{
			Value: proto.String(task.CMD),
		},
		Container: &mesos.ContainerInfo{
			Type: mesos.ContainerInfo_DOCKER.Enum(),
			Docker: &mesos.ContainerInfo_DockerInfo{
				Image:   proto.String(task.Image),
				Network: mesos.ContainerInfo_DockerInfo_BRIDGE.Enum(), // Run everything isolated.
			},
		},
	}
}

// Determine if the remaining space inside of the offer is enough for this
// the task we need to create. If it is, create a TaskInfo and return it.
func (s *MaxGreedyMins) CheckFit(
	i int,
	task def.Task,
	wattsConsideration float64,
	offer *mesos.Offer,
	totalCPU *float64,
	totalRAM *float64,
	totalWatts *float64) (bool, *mesos.TaskInfo) {

	// Does the task fit.
	if s.takeOffer(offer, task, *totalCPU, *totalRAM, *totalWatts) {

		*totalWatts += wattsConsideration
		*totalCPU += task.CPU
		*totalRAM += task.RAM
		log.Println("Co-Located with: ")
		coLocated(s.running[offer.GetSlaveId().GoString()])

		taskToSchedule := s.newTask(offer, task)

		fmt.Println("Inst: ", *task.Instances)
		s.schedTrace.Print(offer.GetHostname() + ":" + taskToSchedule.GetTaskId().GetValue())
		*task.Instances--

		if *task.Instances <= 0 {
			// All instances of task have been scheduled, remove it.
			s.tasks = append(s.tasks[:i], s.tasks[i+1:]...)

			if len(s.tasks) <= 0 {
				log.Println("Done scheduling all tasks")
				close(s.Shutdown)
			}
		}

		return true, taskToSchedule
	}

	return false, nil
}

func (s *MaxGreedyMins) ResourceOffers(driver sched.SchedulerDriver, offers []*mesos.Offer) {
	log.Printf("Received %d resource offers", len(offers))

	for _, offer := range offers {
		offerUtils.UpdateEnvironment(offer)
		select {
		case <-s.Shutdown:
			log.Println("Done scheduling tasks: declining offer on [", offer.GetHostname(), "]")
			driver.DeclineOffer(offer.Id, mesosUtils.LongFilter)

			log.Println("Number of tasks still running: ", s.tasksRunning)
			continue
		default:
		}

		tasks := []*mesos.TaskInfo{}

		offerTaken := false
		totalWatts := 0.0
		totalCPU := 0.0
		totalRAM := 0.0

		// Assumes s.tasks is ordered in non-decreasing median max peak order.

		// Attempt to schedule a single instance of the heaviest workload available first.
		// Start from the back until one fits.
		for i := len(s.tasks) - 1; i >= 0; i-- {

			task := s.tasks[i]
			wattsConsideration, err := def.WattsToConsider(task, s.classMapWatts, offer)
			if err != nil {
				// Error in determining wattsConsideration.
				log.Fatal(err)
			}

			// Don't take offer if it doesn't match our task's host requirement.
			if offerUtils.HostMismatch(*offer.Hostname, task.Host) {
				continue
			}

			// TODO: Fix this so index doesn't need to be passed
			taken, taskToSchedule := s.CheckFit(i, task, wattsConsideration, offer,
				&totalCPU, &totalRAM, &totalWatts)

			if taken {
				offerTaken = true
				tasks = append(tasks, taskToSchedule)
				break
			}
		}

		// Pack the rest of the offer with the smallest tasks.
		for i := 0; i < len(s.tasks); i++ {
			task := s.tasks[i]
			wattsConsideration, err := def.WattsToConsider(task, s.classMapWatts, offer)
			if err != nil {
				// Error in determining wattsConsideration.
				log.Fatal(err)
			}

			// Don't take offer if it doesn't match our task's host requirement.
			if offerUtils.HostMismatch(*offer.Hostname, task.Host) {
				continue
			}

			for *task.Instances > 0 {
				// TODO: Fix this so index doesn't need to be passed
				taken, taskToSchedule := s.CheckFit(i, task, wattsConsideration, offer,
					&totalCPU, &totalRAM, &totalWatts)

				if taken {
					offerTaken = true
					tasks = append(tasks, taskToSchedule)
				} else {
					break // Continue on to next task.
				}
			}
		}

		if offerTaken {
			log.Printf("Starting on [%s]\n", offer.GetHostname())
			driver.LaunchTasks([]*mesos.OfferID{offer.Id}, tasks, mesosUtils.DefaultFilter)
		} else {

			// If there was no match for the task.
			fmt.Println("There is not enough resources to launch a task:")
			cpus, mem, watts := offerUtils.OfferAgg(offer)

			log.Printf("<CPU: %f, RAM: %f, Watts: %f>\n", cpus, mem, watts)
			driver.DeclineOffer(offer.Id, mesosUtils.DefaultFilter)
		}
	}
}

func (s *MaxGreedyMins) StatusUpdate(driver sched.SchedulerDriver, status *mesos.TaskStatus) {
	log.Printf("Received task status [%s] for task [%s]", NameFor(status.State), *status.TaskId.Value)

	if *status.State == mesos.TaskState_TASK_RUNNING {
		s.tasksRunning++
	} else if IsTerminal(status.State) {
		delete(s.running[status.GetSlaveId().GoString()], *status.TaskId.Value)
		s.tasksRunning--
		if s.tasksRunning == 0 {
			select {
			case <-s.Shutdown:
				close(s.Done)
			default:
			}
		}
	}
	log.Printf("DONE: Task status [%s] for task [%s]", NameFor(status.State), *status.TaskId.Value)
}
