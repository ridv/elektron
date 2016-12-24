package schedulers

import (
	"bitbucket.org/sunybingcloud/electron/def"
	"fmt"
	"github.com/golang/protobuf/proto"
	mesos "github.com/mesos/mesos-go/mesosproto"
	"github.com/mesos/mesos-go/mesosutil"
	sched "github.com/mesos/mesos-go/scheduler"
	"log"
	"sort"
	"strings"
	"time"
)

// Decides if to take an offer or not
func (*BPSWClassMapWatts) takeOffer(offer *mesos.Offer, task def.Task) bool {

	cpus, mem, watts := OfferAgg(offer)

	//TODO: Insert watts calculation here instead of taking them as a parameter

	if cpus >= task.CPU && mem >= task.RAM && watts >= task.Watts {
		return true
	}

	return false
}

type BPSWClassMapWatts struct {
	tasksCreated int
	tasksRunning int
	tasks        []def.Task
	metrics      map[string]def.Metric
	running      map[string]map[string]bool
	ignoreWatts  bool

	// First set of PCP values are garbage values, signal to logger to start recording when we're
	// about to schedule a new task
	RecordPCP bool

	// This channel is closed when the program receives an interrupt,
	// signalling that the program should shut down.
	Shutdown chan struct{}
	// This channel is closed after shutdown is closed, and only when all
	// outstanding tasks have been cleaned up
	Done chan struct{}

	// Controls when to shutdown pcp logging
	PCPLog chan struct{}
}

// New electron scheduler
func NewBPSWClassMapWatts(tasks []def.Task, ignoreWatts bool) *BPSWClassMapWatts {
	sort.Sort(def.WattsSorter(tasks))

	s := &BPSWClassMapWatts{
		tasks:       tasks,
		ignoreWatts: ignoreWatts,
		Shutdown:    make(chan struct{}),
		Done:        make(chan struct{}),
		PCPLog:      make(chan struct{}),
		running:     make(map[string]map[string]bool),
		RecordPCP:   false,
	}
	return s
}

func (s *BPSWClassMapWatts) newTask(offer *mesos.Offer, task def.Task) *mesos.TaskInfo {
	taskName := fmt.Sprintf("%s-%d", task.Name, *task.Instances)
	s.tasksCreated++

	if !s.RecordPCP {
		// Turn on logging
		s.RecordPCP = true
		time.Sleep(1 * time.Second) // Make sure we're recording by the time the first task starts
	}

	// If this is our first time running into this Agent
	if _, ok := s.running[offer.GetSlaveId().GoString()]; !ok {
		s.running[offer.GetSlaveId().GoString()] = make(map[string]bool)
	}

	// Add task to list of tasks running on node
	s.running[offer.GetSlaveId().GoString()][taskName] = true

	resources := []*mesos.Resource{
		mesosutil.NewScalarResource("cpus", task.CPU),
		mesosutil.NewScalarResource("mem", task.RAM),
	}

	if !s.ignoreWatts {
		resources = append(resources, mesosutil.NewScalarResource("watts", task.Watts))
	}

	return &mesos.TaskInfo{
		Name: proto.String(taskName),
		TaskId: &mesos.TaskID{
			Value: proto.String("electron-" + taskName),
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
				Network: mesos.ContainerInfo_DockerInfo_BRIDGE.Enum(), // Run everything isolated
			},
		},
	}
}

func (s *BPSWClassMapWatts) Registered(
	_ sched.SchedulerDriver,
	frameworkID *mesos.FrameworkID,
	masterInfo *mesos.MasterInfo) {
	log.Printf("Framework %s registered with master %s", frameworkID, masterInfo)
}

func (s *BPSWClassMapWatts) Reregistered(_ sched.SchedulerDriver, masterInfo *mesos.MasterInfo) {
	log.Printf("Framework re-registered with master %s", masterInfo)
}

func (s *BPSWClassMapWatts) Disconnected(sched.SchedulerDriver) {
	log.Println("Framework disconnected with master")
}

func (s *BPSWClassMapWatts) ResourceOffers(driver sched.SchedulerDriver, offers []*mesos.Offer) {
	log.Printf("Received %d resource offers", len(offers))

	for _, offer := range offers {
		select {
		case <-s.Shutdown:
			log.Println("Done scheduling tasks: declining offer on [", offer.GetHostname(), "]")
			driver.DeclineOffer(offer.Id, longFilter)

			log.Println("Number of tasks still running: ", s.tasksRunning)
			continue
		default:
		}

		tasks := []*mesos.TaskInfo{}

		offer_cpu, offer_ram, offer_watts := OfferAgg(offer)

		taken := false
		totalWatts := 0.0
		totalCPU := 0.0
		totalRAM := 0.0
		for i, task := range s.tasks {

			// Check host if it exists
			if task.Host != "" {
				// Don't take offer if it doesn't match our task's host requirement
				if !strings.HasPrefix(*offer.Hostname, task.Host) {
					continue
				}
			}

			for *task.Instances > 0 {
				var nodeClass string
				for _, attr := range offer.GetAttributes() {
					if attr.GetName() == "class" {
						nodeClass = attr.GetText().GetValue()
					}
				}
				// Does the task fit
				if (s.ignoreWatts || offer_watts >= (totalWatts+task.ClassToWatts[nodeClass])) &&
					(offer_cpu >= (totalCPU + task.CPU)) &&
					(offer_ram >= (totalRAM + task.RAM)) {

					fmt.Println("Watts being used: ", task.ClassToWatts[nodeClass])
					taken = true
					totalWatts += task.ClassToWatts[nodeClass]
					totalCPU += task.CPU
					totalRAM += task.RAM
					log.Println("Co-Located with: ")
					coLocated(s.running[offer.GetSlaveId().GoString()])
					tasks = append(tasks, s.newTask(offer, task))

					fmt.Println("Inst: ", *task.Instances)
					*task.Instances--

					if *task.Instances <= 0 {
						// All instances of task have been scheduled, remove it
						s.tasks = append(s.tasks[:i], s.tasks[i+1:]...)

						if len(s.tasks) <= 0 {
							log.Println("Done scheduling all tasks")
							close(s.Shutdown)
						}
					}
				} else {
					break // Continue on to next offer
				}
			}
		}

		if taken {
			log.Printf("Starting on [%s]\n", offer.GetHostname())
			driver.LaunchTasks([]*mesos.OfferID{offer.Id}, tasks, defaultFilter)
		} else {

			// If there was no match for the task
			fmt.Println("There is not enough resources to launch a task:")
			cpus, mem, watts := OfferAgg(offer)

			log.Printf("<CPU: %f, RAM: %f, Watts: %f>\n", cpus, mem, watts)
			driver.DeclineOffer(offer.Id, defaultFilter)
		}
	}
}

func (s *BPSWClassMapWatts) StatusUpdate(driver sched.SchedulerDriver, status *mesos.TaskStatus) {
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

func (s *BPSWClassMapWatts) FrameworkMessage(
	driver sched.SchedulerDriver,
	executorID *mesos.ExecutorID,
	slaveID *mesos.SlaveID,
	message string) {

	log.Println("Getting a framework message: ", message)
	log.Printf("Received a framework message from some unknown source: %s", *executorID.Value)
}

func (s *BPSWClassMapWatts) OfferRescinded(_ sched.SchedulerDriver, offerID *mesos.OfferID) {
	log.Printf("Offer %s rescinded", offerID)
}
func (s *BPSWClassMapWatts) SlaveLost(_ sched.SchedulerDriver, slaveID *mesos.SlaveID) {
	log.Printf("Slave %s lost", slaveID)
}
func (s *BPSWClassMapWatts) ExecutorLost(_ sched.SchedulerDriver, executorID *mesos.ExecutorID, slaveID *mesos.SlaveID, status int) {
	log.Printf("Executor %s on slave %s was lost", executorID, slaveID)
}

func (s *BPSWClassMapWatts) Error(_ sched.SchedulerDriver, err string) {
	log.Printf("Receiving an error: %s", err)
}
