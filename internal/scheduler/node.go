package scheduler

import "time"

// Node represents a worker machine in our cluster
// We don't use the Proto struct directly because we need extra fields
// like LastHeartbeat that aren't sent over the network
type Node struct {
	ID          string
	IP          string
	CPUCores    int
	MemoryBytes int64

	// Status tracking (critical for fault tolerance - ability to continue operating even when a component fails)
	LastHeartbeat time.Time // master calculates this itnernall to decide if a Node is dead
	Status        NodeStatus
}

type NodeStatus string

// typed enumerations
const (
	StatusHealthy NodeStatus = "HEALTHY"
	StatusDead    NodeStatus = "DEAD"
)
