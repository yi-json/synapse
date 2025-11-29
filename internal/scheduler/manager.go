package scheduler

import (
	"errors"
	"sync"
	"time"
)

// ClusterManager defines the operations we can perform on our worker pool
// Using an interface allows us to swap this for a database implementation later
type ClusterManager interface {
	RegisterNode(n *Node) error
	UpdateHeartbeat(nodeID string) error
	GetNode(nodeId string) (*Node, error)
	MarkDeadNodes(timeout time.Duration) []string
	SubmitJob(j *Job)
	GetPendingJobs() []*Job
	Schedule() []*Job
}

// InMemoryCluster stores node state in a Go map protected by a mutex
type InMemoryCluster struct {
	nodes map[string]*Node

	// RWMutex allows multiple readers (Scheduling) but only one writer (Registration)
	// reading (scheduling jobs) happens way more than writing (registering nodes)
	mu sync.RWMutex

	// we use slice as a FIFO queue
	jobQueue []*Job
}

func NewInMemoryCluster() *InMemoryCluster {
	return &InMemoryCluster{
		nodes:    make(map[string]*Node),
		jobQueue: make([]*Job, 0),
	}
}

func (c *InMemoryCluster) RegisterNode(n *Node) error {
	c.mu.Lock() // WRITE LOCK: Block everyone else
	defer c.mu.Unlock()

	if _, exists := c.nodes[n.ID]; exists {
		return errors.New("node already registered")
	}
	n.Status = StatusHealthy
	n.LastHeartbeat = time.Now()
	c.nodes[n.ID] = n
	return nil
}

// updates the last-seen timestamp for a worker
func (c *InMemoryCluster) UpdateHeartbeat(nodeID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	node, exists := c.nodes[nodeID]
	if !exists {
		return errors.New("node not found")
	}
	node.LastHeartbeat = time.Now()
	return nil
}

// retrieves a worker node by its ID
// we use RLock (Read Lock) because we are only reading, allowing multiple concurrent reads
func (c *InMemoryCluster) GetNode(nodeID string) (*Node, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	node, exists := c.nodes[nodeID]
	if !exists {
		return nil, errors.New("node not fouind")
	}
	return node, nil
}

// scans the cluster and marks silent nodes as DEAD
// returns a list of node IDs that were just killed
func (c *InMemoryCluster) MarkDeadNodes(timeout time.Duration) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	deadNodes := []string{}
	now := time.Now()

	for id, node := range c.nodes {
		// skip already dead nodes
		if node.Status == StatusDead {
			continue
		}

		// check if the last heartbeat was too long ago
		if now.Sub(node.LastHeartbeat) > timeout {
			node.Status = StatusDead
			deadNodes = append(deadNodes, id)
		}
	}
	return deadNodes
}

// adds a job to the queue in PENDING state
func (c *InMemoryCluster) SubmitJob(j *Job) {
	c.mu.Lock()
	defer c.mu.Unlock()

	j.Status = JobPending
	c.jobQueue = append(c.jobQueue, j)
}

// returns all jobs waiting to be scheduled
func (c *InMemoryCluster) GetPendingJobs() []*Job {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.jobQueue
}

// attempts to assign pending jobs to available workers
// Gang Scheduling: either the job gets ALL its resources, or it waits
func (c *InMemoryCluster) Schedule() []*Job {
	c.mu.Lock()
	defer c.mu.Unlock()

	var scheduledJobs []*Job

	for _, job := range c.jobQueue {
		if job.Status != JobPending {
			continue // skip jobs already running or failed
		}

		// 1. can we satisfy this job's requirements?
		// find a set of nodes that have enough free CPU/GPU
		neededCPU := job.MinCPU
		neededGPU := job.MinGPU

		// keep track of which nodes we want to use
		candidateNodes := []*Node{}

		for _, node := range c.nodes {
			if node.Status == StatusDead {
				continue
			}

			// does this node have any space?
			if node.AvailableCPU() > 0 || (neededGPU > 0 && node.AvailableGPU() > 0) {
				candidateNodes = append(candidateNodes, node)
				neededCPU -= node.AvailableCPU()
				neededGPU -= node.AvailableGPU()
			}

			// if we found enough, stop searching
			if neededCPU <= 0 && neededGPU <= 0 {
				break
			}
		}

		// 2. decision time
		// if neededCPU > 0, it means the whole cluster combined doesn't have space
		// Gang Scheduling says: DON'T START, wait for more nodes
		if neededCPU > 0 || neededGPU > 0 {
			continue
		}

		// 3. commit (the "all" part of all-or-nothing)
		// we have enough resources. now we actually update the node state
		// NOTE: a smarter scheduler would bin-pack; we are just grabbing resources greedily
		cpuLeftToAssign := job.MinCPU

		for _, node := range candidateNodes {
			if cpuLeftToAssign <= 0 {
				break
			}

			// how much can we taake for this node?
			canTake := node.AvailableCPU()
			if canTake > cpuLeftToAssign {
				canTake = cpuLeftToAssign
			}

			// commit the change
			node.UsedCPU += canTake
			cpuLeftToAssign -= canTake
			job.AssignedNodes = append(job.AssignedNodes, node.ID)
		}
		job.Status = JobScheduled
		scheduledJobs = append(scheduledJobs, job)
	}
	return scheduledJobs
}
