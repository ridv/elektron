package def

import (
	"github.com/mdesenfants/gokmeans"
	"sort"
)

// Information about a cluster of tasks
type TaskCluster struct {
	ClusterIndex int
	Tasks        []Task
	SizeScore    int // How many other clusters is this cluster bigger than
}

// Classification of Tasks using KMeans clustering using the watts consumption observations
type TasksToClassify []Task

func (tc TasksToClassify) ClassifyTasks(numberOfClusters int, taskObservation func(task Task) []float64) []TaskCluster {
	clusters := make(map[int][]Task)
	observations := getObservations(tc, taskObservation)
	// TODO: Make the max number of rounds configurable based on the size of the workload
	// The max number of rounds (currently defaulted to 100) is the number of iterations performed to obtain
	// distinct clusters. When the data size becomes very large, we would need more iterations for clustering.
	if trained, centroids := gokmeans.Train(observations, numberOfClusters, 100); trained {
		for i := 0; i < len(observations); i++ {
			observation := observations[i]
			classIndex := gokmeans.Nearest(observation, centroids)
			if _, ok := clusters[classIndex]; ok {
				clusters[classIndex] = append(clusters[classIndex], tc[i])
			} else {
				clusters[classIndex] = []Task{tc[i]}
			}
		}
	}
	return labelAndOrder(clusters, numberOfClusters, taskObservation)
}

// record observations
func getObservations(tasks []Task, taskObservation func(task Task) []float64) []gokmeans.Node {
	observations := []gokmeans.Node{}
	for i := 0; i < len(tasks); i++ {
		observations = append(observations, taskObservation(tasks[i]))
	}
	return observations
}

// Size tasks based on the power consumption
// TODO: Size the cluster in a better way other than just taking an aggregate of the watts resource requirement.
func clusterSize(tasks []Task, taskObservation func(task Task) []float64) float64 {
	size := 0.0
	for _, task := range tasks {
		for _, observation := range taskObservation(task) {
			size += observation
		}
	}
	return size
}

// Order clusters in increasing order of task heaviness
func labelAndOrder(clusters map[int][]Task, numberOfClusters int, taskObservation func(task Task) []float64) []TaskCluster {
	// Determine the position of the cluster in the ordered list of clusters
	sizedClusters := []TaskCluster{}

	// Initializing
	for i := 0; i < numberOfClusters; i++ {
		sizedClusters = append(sizedClusters, TaskCluster{
			ClusterIndex: i,
			Tasks:        clusters[i],
			SizeScore:    0,
		})
	}

	for i := 0; i < numberOfClusters-1; i++ {
		// Sizing the current cluster
		sizeI := clusterSize(clusters[i], taskObservation)

		// Comparing with the other clusters
		for j := i + 1; j < numberOfClusters; j++ {
			sizeJ := clusterSize(clusters[j], taskObservation)
			if sizeI > sizeJ {
				sizedClusters[i].SizeScore++
			} else {
				sizedClusters[j].SizeScore++
			}
		}
	}

	// Sorting the clusters based on sizeScore
	sort.SliceStable(sizedClusters, func(i, j int) bool {
		return sizedClusters[i].SizeScore <= sizedClusters[j].SizeScore
	})
	return sizedClusters
}
