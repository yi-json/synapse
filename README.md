# Synapse
A distributed system designed to solve the single biggest problem in AI Infrastructure: **scheduling massive, multi-machine training jobs without wasting resources**.

## Background
Standard web servers (like a backend) are happy to to start one by one. If you ask for 50 servers and get 40, your website still works

**AI is different**. If you are training a massive model (like Llama 3) across 64 GPUs, and you can only get 63, the job **cannot start**. Standard schedulers will reserve those 63 GPUs and let them sit idle while waiting for the last one. This wastes millions of dollars in compute time

## Solution: Gang Scheduling
**Idea**: If a user wants 64 GPUs and I only have 63, I will not schedule any of them. I will keep them free for smaller jobs until I can guarantee all 64 at once

### Topology Awareness
In a massive data center, the speed of light matters. If `Node A` and `Node B` are on the same rack, they can talk instantly. If they are on opposite sides of the building.
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
    - **Tech:** A CLI tool (`synctk submit job.yaml`)  
    - **Real-World Equivalent:** **kubectl**

    

## Documentation
Documenting all of the steps I've done for my future self.

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

#### 4. Entry Point
We are going to build in **three logical chunks** in `cmd/scheduler/main.go`:
1. Listen on a TCP port
2. Initialize the internal logic - `ClusterManager`
3. Translate external gRPC requests into internal function calls

##### Chunk 1: Imports & the Server Struct
We define a `SchedulerServer` struct. Notice that it contains an interface (cluster), not a concrete map. This is Dependency Injection - a key pattern for testability at Google.
```go
package scheduler

import (

	// import generated Protobuf code - The "Language" we speak
	pb "github.com/yi-json/synapse/api/proto/v1"

	// import our internal logic - the "Brain"
	"github.com/yi-json/synapse/internal/scheduler"
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

## Resources
    * [Protocol Buffers in Go](https://protobuf.dev/getting-started/gotutorial/)