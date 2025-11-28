package scheduler

// JobStatus tracks where the job is in its lifecycle
type JobStatus string

const (
	JobPending   JobStatus = "PENDING"
	JobScheduled JobStatus = "SCHEDULED"
	JobRunning   JobStatus = "RUNNING"
)

// Job represents a user submission in our system
type Job struct {
	ID        string
	Image     string
	MinCPU    int
	MinMemory int64
	MinGPU    int // The AI requirement

	Status JobStatus

	// Which nodes are running this job? (Empty until Scheduled)
	AssignedNodes []string
}
