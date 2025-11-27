# Synapse
A distributed system designed to solve the single biggest problem in AI Infrastructure: **scheduling massive, multi-machine training jobs without wasting resources**.

## Background
Standard web servers (like a backend) are happy to to start one by one. If you ask for 50 servers and get 40, your website still works

**AI is different**. If you are training a massive model (like Llama 3) across 64 GPUs, and you can only get 63, the job **cannot start**. Standard schedulers will reserve those 63 GPUs and let them sit idle while waiting for the last one. This wastes millions of dollars in compute time.

## Solution: Gang Scheduling
**Idea**: If a user wants 64 GPUs and I only have 63, I will not schedule any of them. I will keep them free for smaller jobs until I can guarantee all 64 at once

### Topology Awareness
In a massive data center, the speed of light matters. If `Node A` and `Node B` are on the same rack, they can talk instantly. If they are on opposite sides of the building, **latency destroys performance**.
    * **Feature**: The scheduler won't just look for ***any*** free 4 CPUs. It will try to find 4 free CPUs on the **same rack** (or simulated grouping) to maximize training speed


### Architecture

We are building three distinct components that mimic Google's **Borg** architecture:

1. **The Brain: Scheduler**
    - **Role:** Holds the “state of the world.” It knows which nodes are free, which are busy, and where they are physically located  
    - **Tech:** Go, gRPC, Mutexes (for thread safety)  
    - **Real-World Equivalent:** **BorgMaster** or **Kubernetes Scheduler**

2. **The Muscle: Worker Node**
    - **Role:** A lightweight agent on every machine. It listens for orders (“Start Job 50”) and executes them  
    - **Tech:** Go (agent) calling [Rust Carapace (runtime)](https://github.com/yi-json/carapace)  
    - **Real-World Equivalent:** **Borglet** or **Kubelet**

3. **The Interface: CLI**
    - **Role:** How users submit work  
    - **Tech:** A CLI tool (`synctl submit job.yaml`)  
    - **Real-World Equivalent:** **kubectl**

    

## Documentation
Documenting all of the steps I've done for my future self.

<details>
<summary>Click to see the Step-by-Step Implementation Log</summary>

### Phase 0: Setting up the Environment
Every Go project needs a `go.mod` file that tracks dependencies
```bash
go mod init github.com/yourusername/myproject
```

Prerequisite Setup:
```bash
# 1. Install System Tools
sudo apt update && sudo apt install -y golang-go protobuf-compiler
export PATH="$PATH:$(go env GOPATH)/bin"

# 2. Install Proto Plugins
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# 3. Install Dependencies
# This automatically reads your code and downloads everything needed.
go mod tidy
```

### Phase 1: The Cluster Foundation (Connectivity)

**Goal:** Build a “dumb” cluster that stays connected.

**What to build:**  
- Master node (Scheduler) binary  
- Worker node (Agent) binary  

**Key Feature:**  
- **Heartbeat Monitor** — if a Worker crashes, the Master detects it within **5 seconds** and marks it **DEAD**.


#### Success Criteria
1. Start **1 Master**.
2. Start **3 Workers** (each in separate terminals).
3. Kill **Worker 2**.
4. Master logs: `WARN: Node worker-2 missed heartbeat. Marking DEAD.`


#### 1. Define Internal Models
The `.proto` files that are generated are for **transport** (sending data over the wire). They are clunky to use for logic. We need internal Go structs to represent a `Node` in our system (e.g., to track when we last heard from it)
* File: `internal/scheduler/node.go`

#### 2. Manager: Define State Interface
Before writing the logic, we define ***what*** the logic must do. We need a **"Cluster Manager"** that holds the state of all nodes
* File: `internal/scheduler/manager.go`

Breakdown:
* `RegisterNode`
    * Called when a worker node first joins the cluster
        * Stores the node in some internal registry
        * Returns an error if the node already exists
* `UpdateHeartbeat`
    * Called whenever a node pings the scheduler ("I'm alive")
        * Updates `LastHeartbeat`
        * Used for fault tolerance - if `time.Since(LastHeartbeat` is huge, scheduler marks node as dead
* `GetNode`
    * Retrieves information about a specific node
    * Returns an error if the node doesn't exist

#### 3. Implement Thread-Safe State
Now we implement the interface. Since gRPC requests come in concurrently (multiple workers hitting the master at once), we **must** use a Mutex. Without this, the map will panic and crash the Master
* File: `internal/scheduler/manager.go`

#### 4. Writing the Master Node (Control Plane)
We are going to build in **three logical chunks** in `cmd/scheduler/main.go`:
1. Listen on a TCP port
2. Initialize the internal logic - `ClusterManager`
3. Translate external gRPC requests into internal function calls

##### Chunk 1: Imports & the Server Struct
We define a `SchedulerServer` struct. Notice that it contains an interface (cluster), not a concrete map. This is __Dependency Injection__ - a key pattern for testability at Google.
```go
package main

import (
	"context"
	"log"
	"net"

	// import generated Protobuf code - The "Language" we speak
	pb "github.com/yi-json/synapse/api/proto/v1"

	// import our internal logic - the "Brain"
	"github.com/yi-json/synapse/internal/scheduler"

	"google.golang.org/grpc"
)

// SchedulerServer wraps our internal logic so it can talk to gRPC
// it doesn't store state directly; iot delegates to the ClusterManager
type SchedulerServer struct {
	pb.UnimplementedSchedulerServer

	// dependency injection: we rely on the interface, not the specific implementation
	cluster scheduler.ClusterManager
}
```

##### Chunk 2: The API Handler
Now we implement the `RegisterWorker` method defined in the `scheduler.proto` file. This acts as the "Translator". It takes the gRPC request, converts it to our internal `Node` struct, and passes it to the manager
```go
// handles initial handshake from a new worker node
func (s *SchedulerServer) RegisterWorker(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	// log the incoming request - observability
	log.Printf("[GRPC] RegisterWorker: ID=%s, CPU=%d, RAM=%d", req.WorkerId, req.CpuCores, req.MemoryBytes)

	// convert the proto (wire format) -> internal Node struct (domain object)
	newNode := &scheduler.Node{
		ID:          req.WorkerId,
		CPUCores:    int(req.CpuCores),
		MemoryBytes: req.MemoryBytes,
	}

	// delegate to the business logic layer
	err := s.cluster.RegisterNode(newNode)
	if err != nil {
		log.Printf("[ERROR] Failed to register node: %v", err)
		return &pb.RegisterResponse{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	// success
	return &pb.RegisterResponse{
		Success: true,
		Message: "Welcome to Synapse",
	}, nil
}
```
##### Chunk 3: The Main Entry Point
Finally, we wire everything together. We spin up the TCP listener, create the in-memory cluster, and start the gRPC server
```go
func main() {
	// setup networking: listen on TCP port 9000
	lis, err := net.Listen("tcp", ":9000")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	// initialize the logic: create the state store
	clusterManager := scheduler.NewInMemoryCluster()

	// initialize the server: inject the logic into the new gRPC server
	grpcServer := grpc.NewServer()
	schedulerServer := &SchedulerServer{
		cluster: clusterManager,
	}

	// register the service so gRPC knows where to send requests
	pb.RegisterSchedulerServer(grpcServer, schedulerServer)

	// start blocking loop
	log.Printf("Synapse Master running on :9000...")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
```

With these changes, we can run the following to test if we have build a connected cluster
```bash
go run cmd/scheduler/main.go
```

with the expected output:
```bash
2025/11/27 16:25:00 Synapse Master running on :9000...
```
It should hang (i.e not run anything else) because the line `grpcServer.Serve(lis)` starts an infinite loop that listens for incoming network connections. It will sit there forever until you press `Ctrl+C` to stop it

Next steps? Leave the terminal open, open a **NEW** terminal window and we will start working on the **Worker** node to connect to this Master

</details>

## Resources
* [Protocol Buffers in Go](https://protobuf.dev/getting-started/gotutorial/)