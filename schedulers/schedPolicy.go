package schedulers

import (
	"bitbucket.org/sunybingcloud/electron/def"
	mesos "github.com/mesos/mesos-go/api/v0/mesosproto"
	sched "github.com/mesos/mesos-go/api/v0/scheduler"
	"log"
	"time"
)

type SchedPolicyContext interface {
	// Change the state of scheduling.
	SwitchSchedPol(s SchedPolicyState)
}

type SchedPolicyState interface {
	// Define the particular scheduling policy's methodology of resource offer consumption.
	ConsumeOffers(SchedPolicyContext, sched.SchedulerDriver, []*mesos.Offer)
	// Get information about the scheduling policy.
	GetInfo() (info struct {
		taskDist    float64
		varCpuShare float64
	})
	// Switch scheduling policy if necessary.
	SwitchIfNecessary(SchedPolicyContext)
}

type baseSchedPolicyState struct {
	SchedPolicyState
	// Keep track of the number of tasks that have been scheduled.
	numTasksScheduled int
	// Distribution of tasks that the scheduling policy is most appropriate for.
	// This distribution corresponds to the ratio of low power consuming tasks to
	// high power consuming tasks.
	TaskDistribution float64 `json:"taskDist"`
	// The average variance in cpu-share per task that this scheduling policy can cause.
	// Note: This number corresponds to a given workload.
	VarianceCpuSharePerTask float64 `json:"varCpuShare"`
}

func (bsps *baseSchedPolicyState) nextPolicy(baseSchedRef *BaseScheduler) (string, float64) {
	// Name of the scheduling policy to switch to.
	switchToPolicyName := ""
	// Record overhead to classify the tasks in the scheduling window and using the classification results
	// 	to determine the distribution of low power consuming and high power consuming tasks.
	startTime := time.Now()
	// Determine the distribution of tasks in the new scheduling window.
	taskDist, err := def.GetTaskDistributionInWindow(baseSchedRef.schedWindowSize, baseSchedRef.tasks)
	baseSchedRef.LogClsfnAndTaskDistOverhead(time.Now().Sub(startTime))
	if err != nil {
		// All the tasks in the window were only classified into 1 cluster.
		// Max-Min and Max-GreedyMins would work the same way as Bin-Packing for this situation.
		// So, we have 2 choices to make. First-Fit or Bin-Packing.
		// If choose Bin-Packing, then there might be a performance degradation due to increase in
		// 	resource contention. So, First-Fit might be a better option to cater to the worst case
		// 	where all the tasks are power intensive tasks.
		// TODO: Another possibility is to do the exact opposite and choose Bin-Packing.
		// TODO[2]: Determine scheduling policy based on the distribution of tasks in the whole queue.
		switchToPolicyName = bp
	} else {
		// The tasks in the scheduling window were classified into 2 clusters, meaning that there is
		// 	some variety in the kind of tasks.
		// We now select the scheduling policy which is most appropriate for this distribution of tasks.
		first := schedPoliciesToSwitch[0]
		last := schedPoliciesToSwitch[len(schedPoliciesToSwitch)-1]
		if taskDist < first.sp.GetInfo().taskDist {
			switchToPolicyName = first.spName
		} else if taskDist > last.sp.GetInfo().taskDist {
			switchToPolicyName = last.spName
		} else {
			low := 0
			high := len(schedPoliciesToSwitch) - 1
			for low <= high {
				mid := (low + high) / 2
				if taskDist < schedPoliciesToSwitch[mid].sp.GetInfo().taskDist {
					high = mid - 1
				} else if taskDist > schedPoliciesToSwitch[mid].sp.GetInfo().taskDist {
					low = mid + 1
				} else {
					switchToPolicyName = schedPoliciesToSwitch[mid].spName
					break
				}
			}
			// We're here if low == high+1.
			// If haven't yet found the closest match.
			if switchToPolicyName == "" {
				lowDiff := schedPoliciesToSwitch[low].sp.GetInfo().taskDist - taskDist
				highDiff := taskDist - schedPoliciesToSwitch[high].sp.GetInfo().taskDist
				if lowDiff > highDiff {
					switchToPolicyName = schedPoliciesToSwitch[high].spName
				} else if highDiff > lowDiff {
					switchToPolicyName = schedPoliciesToSwitch[low].spName
				} else {
					// index doens't matter as the values at high and low are equidistant
					// 	from taskDist.
					switchToPolicyName = schedPoliciesToSwitch[high].spName
				}
			}
		}
	}
	return switchToPolicyName, taskDist
}

func (bsps *baseSchedPolicyState) SwitchIfNecessary(spc SchedPolicyContext) {
	baseSchedRef := spc.(*BaseScheduler)
	// Switching scheduling policy only if feature enabled from CLI.
	if baseSchedRef.schedPolSwitchEnabled {
		// Name of scheduling policy to switch to.
		switchToPolicyName := ""
		// Distribution of tasks in the scheduling window
		var taskDist float64
		// If scheduling window has not been fixed, then determine the scheduling window based on the current
		// 	availability of resources on the cluster (Mesos perspective).
		if !baseSchedRef.toFixSchedWindow {
			// Need to compute the size of the scheduling window.
			// The next scheduling policy will schedule at max schedWindowSize number of tasks.
			baseSchedRef.schedWindowSize, baseSchedRef.numTasksInSchedWindow =
				baseSchedRef.schedWindowResStrategy.Apply(func() interface{} { return baseSchedRef.tasks })
		}

		// Now, we need to switch if the scheduling window is > 0.
		if baseSchedRef.schedWindowSize > 0 {
			// If we haven't received any resource offers, then
			// 	check whether we need to fix the first scheduling policy to deploy.
			// 	If not, then determine the first scheduling policy based on the distribution of tasks
			//		in the scheduling window.
			// Else,
			// 	Check whether the currently deployed scheduling policy has already scheduled the
			// 		schedWindowSize number of tasks.
			// 	If yes, then we switch to the scheduling policy based on the distribution of tasks in
			//		the scheduling window.
			// 	If not, then we continue to use the currently deployed scheduling policy.
			if !baseSchedRef.hasReceivedResourceOffers {
				if baseSchedRef.nameOfFstSchedPolToDeploy != "" {
					switchToPolicyName = baseSchedRef.nameOfFstSchedPolToDeploy
				} else {
					switchToPolicyName, taskDist = bsps.nextPolicy(baseSchedRef)
				}
			} else {
				if bsps.numTasksScheduled >= baseSchedRef.schedWindowSize {
					switchToPolicyName, taskDist = bsps.nextPolicy(baseSchedRef)
				} else {
					// We continue working with the currently deployed scheduling policy.
					log.Println("Continuing with the current scheduling policy...")
					log.Printf("TasksScheduled[%d], SchedWindowSize[%d]", bsps.numTasksScheduled,
						baseSchedRef.schedWindowSize)
					return
				}
			}
		} else {
			// We continue working with the currently deployed scheduling policy.
			log.Println("Continuing with the current scheduling policy...")
			log.Printf("TasksScheduled[%d], SchedWindowSize[%d]", bsps.numTasksScheduled,
				baseSchedRef.schedWindowSize)
			return
		}
		// Switching scheduling policy.
		baseSchedRef.LogSchedPolicySwitch(taskDist, switchToPolicyName, SchedPolicies[switchToPolicyName])
		baseSchedRef.SwitchSchedPol(SchedPolicies[switchToPolicyName])
		// Resetting the number of tasks scheduled as this is a new scheduling policy that has been
		// 	deployed.
		bsps.numTasksScheduled = 0
	}
}

func (bsps *baseSchedPolicyState) GetInfo() (info struct {
	taskDist    float64
	varCpuShare float64
}) {
	info.taskDist = bsps.TaskDistribution
	info.varCpuShare = bsps.VarianceCpuSharePerTask
	return info
}
