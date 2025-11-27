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
}

// InMemoryCluster stores node state in a Go map protected by a mutex
type InMemoryCluster struct {
	nodes map[string]*Node

	// RWMutex allows multiple readers (Scheduling) but only one writer (Registration)
	// reading (scheduling jobs) happens way more than writing (registering nodes)
	mu sync.RWMutex
}

func NewInMemoryCluster() *InMemoryCluster {
	return &InMemoryCluster{
		nodes: make(map[string]*Node),
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
