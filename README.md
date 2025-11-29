# Synapse
A distributed system designed to solve the single biggest problem in AI Infrastructure: **scheduling massive, multi-machine training jobs without wasting resources**.

## Background
Standard web servers (like a backend) are happy to start one by one. If you ask for 50 servers and get 40, your website still works

**AI is different**. If you are training a massive model (like Llama 3) across 64 GPUs, and you can only get 63, the job **cannot start**. Standard schedulers will reserve those 63 GPUs and let them sit idle while waiting for the last one. This wastes millions of dollars in compute time.

## Solution: Gang Scheduling
**Idea**: If a user wants 64 GPUs and I only have 63, I will not schedule any of them. I will keep them free for smaller jobs until I can guarantee all 64 at once

### Topology Awareness
In a massive data center, the speed of light matters. If `Node A` and `Node B` are on the same rack, they can talk instantly. If they are on opposite sides of the building, **latency destroys performance**.
* **Feature**: The scheduler won't just look for ***any*** free 4 GPUs. It will try to find 4 free GPUs on the **same rack** (or simulated grouping) to maximize training speed


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

## Documentation
Documenting all of the steps I've done for my future self.

<details>
<summary>Click to see the Step-by-Step Implementation Log</summary>

### Phase 0: Setting up the Environment
Every Go project needs a `go.mod` file that tracks dependencies
```bash
go mod init github.com/yourusername/myproject
```

List of Libaries I had to download:
```bash
# gRPC framwork (networking)
go get google.golang.org/grpc

# protobuf runtime (serialization)
go get google.golang.org/protobuf

# uuid generator (random IDs)
go get github.com/google/uuid

go mod tidy
```

### Phase 1: The Cluster Foundation (Connectivity)

**Goal:** Build a “dumb” cluster that stays connected. We need a control plane that can accept connections and maintain a thread-safe list of active nodes

**What to build:**  
- Master node (Scheduler) binary  
- Worker node (Agent) binary  

**Key Feature:**  
- **Concurrency Safety**: Since thousands of nodes could theoretically join at once, the Scheduler uses a `sync.RWMutex`to prevent race conditions during registration

#### Success Criteria
- **Protocol Compilation**: Running `protoc` generates `pb.go` files without errors
- **Master Startup**: `go run cmd/scheduler/main.go` logs:
```bash
Synapse Master running on :9000...
```
- **Blocking Behavior**: The master process hangs (listens) and does not exit immediately


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

### Phase 2: The Worker Node & Failure Detection

**Goal:** Establish the **Data Plane** and ensure **Fault Tolerance**. We need a persistent agent on every compute node that executes commands, plus a monitoring system to detect when nodes crash.

**What to build:**  
* **The Worker Protocol**: Defing `worker.proto` so the Master knows how to send commands (like `StartJob` to the Worker)
* **Dual-Role Binary**: The Worker is unqiue because it acts as both:
	- Client: connects to the Master to say "I'm alive" (Heartbeats)
	- Server: listens for commands from the Master ("Start Job #50")
* **The Reaper**: A background loop in the Master that scans for silent nodes. If a node hasn't sent a heartbeat in 10 seconds, it is marked `DEAD`.

**Key Feature:**  
* **Self-Registration Handshake**: Unlike static systems where you have to manually configure a list of IP addresses, Synapse Workers **auto-discover** the Master. A Worker starts up, generates a unique UUID, and announces itself to the cluster dynamically.
* **Distributed Failure Detection**: The system distinguishes between "Idle" nodes and "Dead" nodes. A crashed worker is automatically detected and removed from the healthy pool without human intervention.


#### Success Criteria
1. Registration: Start Master and Worker. Worker logs `Success!` Master says: `Welcome.`
2. Liveness: Verify `[GRPC] Heartbeat from <ID>` appears in Master logs every 5 seconds.
3. The Reaper Test:
	* Let the system run for 10 seconds.
	* Kill the Worker process `(Ctrl+C)`.
4. Verification: Within 10 seconds, the Master must log: `REAPER: Node <ID> is DEAD.`


#### 1. Writing the Worker Node
We are going to build in four logical chunks

##### Chunk 1: Imports and Constants
We set up the file and define where the master lives
* File: `cmd/worker/main.go`
```go
package main

const (
	// where the scheduler (Master) is listening
	MasterAddr = "localhost:9000"

	// the port THIS worker will listen on (we will use this later)
	WorkerPort = 8000
)
```

##### Chunk 2: The Connection Logic
In this chunk, we start the `main` function. We generate a unique ID for the worker (so the master can track it) and establish the TCP connection to the scheduler.
```go
import (
	"log"

	// Generate random IDs for the worker
	"github.com/google/uuid"

	// Import our generated Proto definitions
	pb "github.com/yi-json/synapse/api/proto/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	workerID := uuid.New().String()
	log.Printf("Starting Worker Node - ID: %s", workerID)

	// connect to the master
	// we use "insecure" credentials because we haven't set up SSL/TLS certificates yet
	// this opens a TCP connection to localhost:9000
	conn, err := grpc.NewClient(MasterAddr, grpc.WithTransportCredentials((insecure.NewCredentials())))
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	// "defer" ensures the connection closes when the function exits (cleanup)
	defer conn.Close()

	// create the client stub
	// this "client" object has methods like RegisterWorker() that we can call directly
	client := pb.NewSchedulerClient(conn)
}
```

##### Chunk 3: The Handshake (Register)
In this chunk, we perform the RPC. We create a "Context" with a timeout (so we don't hang forever if the Master is down), construct the request packet, and fire it off.
```go
	// handshake: register with the master
	// we create a context with a 1 second timeout
	// if the master doesn't respond within 1 second, we cancel the request
	// func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc)
	// func Background() Context
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// we send our stats to the Master
	response, err := client.RegisterWorker(ctx, &pb.RegisterRequest{
		WorkerId:    workerID,
		CpuCores:    4,
		MemoryBytes: 1024 * 1024,
		Port:        WorkerPort,
	})

	// critical failure check: if we can't join the cluster, we crash
	if err != nil {
		log.Fatalf("could not register: %v", err)
	}

	log.Printf("Success! Master says: %s", response.Message)
```

Some info about Contexts:
- `Background()` returns a non-nil, empty Context. It is never canceled, has no values, and has no deadline. It is typically used by the main function, initialization, and tests, and as the top-level Context for incoming requests.
- `WithTimeout` returns `WithDeadline(parent, time.Now().Add(timeout))`.

##### Chunk 4: The Infinite Loop - Keeping Alive
Since we scheduled the Registration as a background task (conceptually), if `main()` reaches the end, the program terminates. To be a "Server" or a long-running daemon, we must prevent he program from exiting.
```go
	// keep the process alive
	// this empty select statement block forever without using CPU
	// "wait here until the program is kiled"
	// TODO: replace this with our own gRPC server listener
	select {}
```

To verify that our distributed system actually communicates, we open two terminals:
1. Start the Master (Terminal 1)
```bash
go run cmd/scheduler/main.go
```
Output: `Synapse Master running on :9000...`

2. Start the Worker (Terminal 2)
```bash
go run cmd/worker/main.go
```
Output: `[GRPC] RegisterWorker: ID=..., CPU=4...`

#### 2. The Heartbeat - Pulse Check
Right now, if we kill the Worker, the Master has no idea. We need the Worker to ping the Master every 5 seconds: "I'm still here"

First we define `HeartbeatRequest` and `HeartbeatResponse` and then add the `SendHeartbeat` RPC method:
```proto
service Scheduler {
    rpc RegisterWorker(RegisterRequest) returns (RegisterResponse);
    rpc UpdateHeartbeat(HeartbeatRequest) returns (HeartbeatResponse);
}

...

message HeartbeatRequest {
    string worker_id = 1;
    int32 current_load = 2; // e.g., CPU Usage %
    int32 active_jobs = 3; // # of containers being used
}

message HeartbeatResponse {
    bool acknowledge = 1;
}
```
- File: `api/proto/v1/scheduler.proto`

```go
// allows a worker to ping the master to indicate liveness
func (s *SchedulerServer) SendHeartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	log.Printf("[GRPC] Heartbeat from %s", req.WorkerId)

	// delegate to the internal cluster logic
	err := s.cluster.UpdateHeartbeat(req.WorkerId)
	if err != nil {
		log.Printf("[ERROR] Heartbeat failed for %s: %v", req.WorkerId)
		return nil, err
	}

	// success
	return &pb.HeartbeatResponse{Acknowledge: true}, nil
}
```
- File: `cmd/scheduler/main.go`

To verify that our distributed system actually communicates AND sends heartbeat pulses, we open two terminals:
1. Start the Master (Terminal 1)
```bash
go run cmd/scheduler/main.go
```
Output:
```bash
2025/11/28 11:59:08 Synapse Master running on :9000...
2025/11/28 11:59:12 [GRPC] RegisterWorker: ID=de395d02-64ba-4330-bfce-94a3c662e1a7, CPU=4, RAM=1048576
2025/11/28 11:59:17 [GRPC] Heartbeat from de395d02-64ba-4330-bfce-94a3c662e1a7
2025/11/28 11:59:22 [GRPC] Heartbeat from de395d02-64ba-4330-bfce-94a3c662e1a7
2025/11/28 11:59:27 [GRPC] Heartbeat from de395d02-64ba-4330-bfce-94a3c662e1a7
```

2. Start the Worker (Terminal 2)
```bash
go run cmd/worker/main.go
```
Output:
```bash
2025/11/28 11:59:12 Starting Worker Node - ID: de395d02-64ba-4330-bfce-94a3c662e1a7
2025/11/28 11:59:12 Success! Master says: Welcome to Synapse
2025/11/28 11:59:17 Pulse sent
2025/11/28 11:59:22 Pulse sent
2025/11/28 11:59:27 Pulse sent
```
#### 3. The Reaper - Checks for Silence
THe Master records the heartbeats, but it doesn't **act** on them. If you kill the Worker right now, the Master will just stop logging messages. It won't actually mark the node as `DEAD`
```go
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
```
- File: `internal/mscheduler/manager.go`

Then we implement the method in our `main.go`
```go
	// register the service so gRPC knows where to send requests
	pb.RegisterSchedulerServer(grpcServer, schedulerServer)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		for range ticker.C {
			// check for nodes that haven't spoken in 10 seconds
			deadIDs := clusterManager.MarkDeadNodes(10 * time.Second)

			for _, id := range deadIDs {
				log.Printf("REAPER: Node %s is DEAD (Missed Heartbeats)", id)

			}
		}
	}()

	// start blocking loop
	log.Printf("Synapse Master running on :9000...")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
```
- File `cmd/scheduler/main.go`

This gives us the expected outputs:

Worker Terminal:
```bash
ubuntu@rusty-box:~/github/synapse$ go run cmd/worker/main.go
2025/11/28 12:56:49 Starting Worker Node - ID: 107fdd38-a490-4853-a2e5-251364a43680
2025/11/28 12:56:49 Success! Master says: Welcome to Synapse
2025/11/28 12:56:54 Pulse sent
2025/11/28 12:56:59 Pulse sent
2025/11/28 12:57:04 Pulse sent
^Csignal: interrupt
```

Master Terminal:
```bash
ubuntu@rusty-box:~/github/synapse$ go run cmd/scheduler/main.go
2025/11/28 12:56:43 Synapse Master running on :9000...
2025/11/28 12:56:49 [GRPC] RegisterWorker: ID=107fdd38-a490-4853-a2e5-251364a43680, CPU=4, RAM=1048576
2025/11/28 12:56:54 [GRPC] Heartbeat from 107fdd38-a490-4853-a2e5-251364a43680
2025/11/28 12:56:59 [GRPC] Heartbeat from 107fdd38-a490-4853-a2e5-251364a43680
2025/11/28 12:57:04 [GRPC] Heartbeat from 107fdd38-a490-4853-a2e5-251364a43680
2025/11/28 12:57:18 REAPER: Node 107fdd38-a490-4853-a2e5-251364a43680 is DEAD (Missed Heartbeats)
```

### Phase 3: The Brain - Gang Scheduling
**Goal:** We want to submit a job that requires **4 CPUs**
* If we have 3 workers with 1 CPU each (Total 3), the job should **WAIT** (Pending)
* If we add a 4th worker, the job should **INSTANTLY START** (Scheduled)

This is Gang Scheduling (All or Nothing)

**What to build:**
* **The Job API**: Update `scheduler.proto` to allow users/CLI to call `SubmitJob` with resource requirements (e.g. `min_gpu: 8`)
* **The Job Queue**: An in-memory FIFO queue in the Master that holds jobs that cannot be scheduled yet
* **The Scheduler Loop**: A control loop that runs every second. It iterates through the `Pending` queue and checks: ***"Do I have enough aggregate resources in the cluster to satisfy this request right now?"***
* **Resource Accounting**: Logic to track `Total` vs `Used` CPU/RAM/GPU on every worker

**Key Feature:**
* **Atomic Allocation**: Resources are granted in a transaction. Either the job gets all requested nodes at once, or it gets none. THis prevents "partial allocation" deadlocks common in distributed training

#### Success Criteria
1. **Submit a "Greedy" Job**: Send a request for 8 CPUs when only 4 are available
2. **Verify Pending**: The master logs `Job <ID> added to PENDING queue (Insufficient Resources)`
3. **Scale Up**: Start a second Worker node in a new terminal
4. **Verify Scheduling**: The Master instantly detects the new resources and logs: `Successfully scheduled Job <ID> to Nodes [Worker-A, Worker-B].`

#### Define the Interal Job Model
We need a struct to represent a "Job" inside our Go logic. This is distinct from the Protobuf message
```go
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
```
- File: `internal/scheduler/job.go`

#### Add the Queue to the Cluster Manager
Now we update our "Brain" (the `InMemoryCluter`) to hold these jobs

1. We update the `InMemoryCluster` struct to include a `jobQueue`
```go
// InMemoryCluster stores node state in a Go map protected by a mutex
type InMemoryCluster struct {
	...

	// we use slice as a FIFO queue
	jobQueue []*Job
}

func NewInMemoryCluster() *InMemoryCluster {
	return &InMemoryCluster{
		nodes:    make(map[string]*Node),
		jobQueue: make([]*Job, 0),
	}
}
```
- File: `internal/scheduler/manager.go`


2. Then we add a function that puts jobs into that queue
```go
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
```
- File: `internal/scheduler/manager.go`

#### Wire the gRPC Handler
Now that the internal logic exists, let's expose it to the network so we can actually call it

1. We update `scheduler.proto` to have `SubmitJobRequest` and `SubmitJobResponse` and `SubmitJob` RPC method
```go
service Scheduler {
    ...
    rpc SubmitJob(SubmitJobRequest) returns (SubmitJobResponse);
}
...
message SubmitJobRequest {
  string id = 1;          // Unique Job ID
  string image = 2;       // Docker image (e.g., "pytorch/training:latest")
  int32 min_cpu = 3;      // "I need at least 4 CPUs"
  int64 min_memory = 4;   // "I need at least 1GB RAM"
  int32 min_gpu = 5;      // "I need at least 2 H100s"
}

message SubmitJobResponse {
    string job_id = 1;
    bool success = 2;
    string message = 3;     // e.g., "Job accepted and queued"
}
```

Then we add the methods in the manager
```go
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
```
- File: `internal/scheduler/manager.go`

#### Build the CLI Tool - `synctl`
We will create a command-line tool that acts like `kubectl`. It will connect to the Master and say: ***"Here is a job that needs 4 CPUs and 1 GPU***
```go
package main

import (
	"context"
	"flag"
	"log"
	"time"

	pb "github.com/yi-json/synapse/api/proto/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const MasterAddr = "localhost:9000"

func main() {
	// parse cmd line flags (e.g. ---cpu=4, --gpu=1)
	cpu := flag.Int("cpu", 1, "CPU Cores required")
	mem := flag.Int("mem", 128, "Memory required in MB")
	gpu := flag.Int("gpu", 0, "GPUs required")
	image := flag.String("image", "ubuntu:latest", "Docker image to run")
	flag.Parse()

	// connect to master
	conn, err := grpc.NewClient(MasterAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()
	client := pb.NewSchedulerClient(conn)

	// create the job request
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	jobID := "job-" + time.Now().Format("150405") // Simple ID based on time

	log.Printf("Submitting Job %s...", jobID)
	response, err := client.RegisterWorker(ctx, &pb.RegisterRequest{
		WorkerId:    workerID,
		CpuCores:    4,
		MemoryBytes: 1024 * 1024 * 1024, // 1 GB
		GpuCount:    1,
		Port:        WorkerPort,
	})

	if err != nil {
		log.Fatalf("Submission Failed: %v", err)
	}

	// 4. Print Success
	log.Printf("Master Response: %s (Job ID: %s)", resp.Message, resp.JobId)
}
```
- File: `cmd/synctl/main.go`

Now, we open a Master terminal via `go run cmd/scheduler/main.go`, then run a CLI terminal via `go run cmd/synctl/main.go --cpu=4 --gpu=1` and we get the following expected response:
```bash
ubuntu@rusty-box:~/github/synapse$ go run cmd/synctl/main.go --cpu=4 --gpu=1
2025/11/28 18:07:51 Submitting Job job-180751...
2025/11/28 18:07:51 Master Response: Job queued successfully (Job ID: job-180751)
```

#### Resource Allocation - Update Node to Track Usage
Right now, our `Node` struct knows its capacity (e.g. "I have 4 CPUs"), but it doesn't know its Usage (e.g. "I am using 2 CPUs")
```go
type Node struct {
	ID string
	IP string

	// Capacity (total hardware)
	CPUCores    int
	MemoryBytes int64
	GPUCount    int

	// Usage (what is currently running)
	UsedCPU    int
	UsedMemory int64
	UsedGPU    int

	// Status tracking (critical for fault tolerance - ability to continue operating even when a component fails)
	LastHeartbeat time.Time // master calculates this itnernall to decide if a Node is dead
	Status        NodeStatus
}
...
// helpers to check available resources
func (n *Node) AvailableCPU() int {
	return n.CPUCores - n.UsedCPU
}

func (n *Node) AvailableGPU() int {
	return n.GPUCount - n.UsedGPU
}
```
- File: `internal/scheduler/node.go`

#### Implement the Gang Scheduler
1. Looks at first pending job
2. Scans the cluster to see if there are **enough total resources** to fit it
3. If yes, it "claims" those resorces by updating the `UsedCPU` on the nodes and marks the job as `SCHEDULED`

```go
// attempts to assign pending jobs to available workers
// Gang Scheduling: either the job gets ALL its resources, or it waits
func (c *InMemoryCluster) Schedule() {
	c.mu.Lock()
	defer c.mu.Unlock()

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
				continue
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
	}
}
```
- File: `internal/scheduler/manager.go`

```go
	// scheduler loop
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		for range ticker.C {
			clusterManager.Schedule()

			// log changes for debugging
			for _, job := range clusterManager.GetPendingJobs() {
				if job.Status == "SCHEDULED" {
					log.Printf("SUCCESS: Scheduled Job %s to nodes %v", job.ID, job.AssignedNodes)

				}
			}

		}
	}()
```
- File: `cmd/scheduler/main.go`



Testing our Gang Scheduling Implementation, we achieve this:
1. Master Terminal
```bash
ubuntu@rusty-box:~/github/synapse$ go run cmd/scheduler/main.go
2025/11/28 22:24:08 Synapse Master running on :9000...
2025/11/28 22:24:09 [GRPC] Job Submitted: job-222409 (CPU: 4, GPU: 0)
2025/11/28 22:24:13 [GRPC] RegisterWorker: ID=8e9c24f5-5f11-449a-b68d-ef1e122118a6, CPU=4, RAM=1048576
2025/11/28 22:24:14 SUCCESS: Scheduled Job job-222409 to nodes [8e9c24f5-5f11-449a-b68d-ef1e122118a6]
2025/11/28 22:24:15 SUCCESS: Scheduled Job job-222409 to nodes [8e9c24f5-5f11-449a-b68d-ef1e122118a6]
2025/11/28 22:24:16 SUCCESS: Scheduled Job job-222409 to nodes [8e9c24f5-5f11-449a-b68d-ef1e122118a6]
2025/11/28 22:24:17 SUCCESS: Scheduled Job job-222409 to nodes [8e9c24f5-5f11-449a-b68d-ef1e122118a6]
2025/11/28 22:24:18 SUCCESS: Scheduled Job job-222409 to nodes [8e9c24f5-5f11-449a-b68d-ef1e122118a6]
2025/11/28 22:24:19 [GRPC] Heartbeat from 8e9c24f5-5f11-449a-b68d-ef1e122118a6
```
This effectively demonstrates that Gang Scheduling works:
- T+0s: Job arrives. Resources = 0. Action = WAIT.
- T+4s: Worker arrives. Resources = 4. Action = SCHEDULE.

2. CLI + Worker Terminal
```bash
ubuntu@rusty-box:~/github/synapse$ go run cmd/synctl/main.go --cpu=4
2025/11/28 22:24:09 Submitting Job job-222409...
2025/11/28 22:24:09 Master Response: Job queued successfully (Job ID: job-222409)
ubuntu@rusty-box:~/github/synapse$ go run cmd/worker/main.go
2025/11/28 22:24:13 Starting Worker Node - ID: 8e9c24f5-5f11-449a-b68d-ef1e122118a6
2025/11/28 22:24:13 Success! Master says: Welcome to Synapse
2025/11/28 22:24:19 Pulse sent
```

What did we just accomplish?
- We verified that the scheduler is "All-or-Nothing." When we submitted a job requiring 4 CPUs (while 0 were available), the job correctly sat in PENDING state. It did not try to partially allocate resources.
- The system now tracks UsedCPU vs TotalCPU. It only scheduled the job the instant a Worker with sufficient capacity joined the cluster, proving the **Gang Scheduling** logic works.
- Each worker has hardcoded values of **4 CPU Cores, 1 GPU, and 1 GB of memory**
- If a given job requires **2 GPUs** for example, we only have Worker 1, we wait until another free worker, like Worker 2, is available. Only then we have accumulated enough resources to mark the job as **Scheduled** and assigns the job to **Nodes:[Worker-1, Worker-2]**.

Next Steps?
- **"Split Brain" Problem**: Right now, the Master thinks the job is running by updating the internal database, but the Worker **has no idea**
- In the next phase, we address this. The Master must send a gRPC `StartJob` command to the Worker, and the Worker must execute the actual binary (using the Rust `Carapace` runtime)

### Phase 4: Execution - Carapace Integration
**Goal:** Close the "Split Brain" gap: when the scheduler matches a job, it must send a gRPC `StartJob` command to the specific worker IP/Port

**What to build:**  
- **Worker Execution Handler**: Implement the `StartJob` RPC in the Worker binary. For now, this logs the intent (`STARTING CONTAINER`), serving as the hook where we will call the Rust `Carapace` binary
- **The Dispatcher**: A background routine in the Master that listens for successful scheduling events, looks up the target Worker's IP/Port, and sends the gRPC command to start the workload

**Key Feature:**
- **Active Orchestration**: Unlike simple monitoring systems that passively receive data, Synapse implements **Active Push**. The Master acts as a client to the Worker, dialing into specific nodes to command them, effectively treating the entire cluster as a single programmable computer


#### Success Criteria
1. **Submit Job**: Run `synctl --cpu=4`
2. **Scheduling Event**: Master logs `SUCCESS: Scheduled Job...`
3. **Execution**: The critical check - The **Worker Terminal** must print `STARTING CONTAINER: JobID=job-... Image=ubuntu:latest`

#### Update `Node` to store the Port
```go
type Node struct {
	ID   string
	IP   string
	Port int
	...
}
```
- File: `internal/scheduler/node.go`

#### Save the Port During Registration
We update the `RegisterNode` logic to actually save this file
```go
	newNode := &scheduler.Node{
		ID:          req.WorkerId,
		CPUCores:    int(req.CpuCores),
		MemoryBytes: req.MemoryBytes,
		GPUCount:    int(req.GpuCount), // add this
		Port:        int(req.Port),     // and this
	}
```

#### Create `worker.proto`
Recall that gRPC connections are **one-way streets**. Up until now the traffic only flowed in one direction
- Worker (Client) -> Calls `Register` -> Master (Server)
- Worker (Client) -> Calls `Heartbeat` -> Master (Server)

Since the master was the only one receiving calls, we only needed one service definition (`Scheduler`)

Now, the Master needs to issue a command: "Hey Worker, start this container!". To do this, the Master must become the **Client**, and the Worker must become the **Server**. Hence, we need `worker.proto` to define the API that the Worker listens on

| Proto File | Defines API For... | Who Listens (Server)? | Who Calls (Client)? |
| :--- | :--- | :--- | :--- |
| **`scheduler.proto`** | Port **9000** | **The Master** | Workers & CLI |
| **`worker.proto`** | Port **8080** | **The Worker** | The Master |

```proto
syntax = "proto3";

package v1;

option go_package = "github.com/yi-json/synapse/api/proto/v1";

// The "Data Plane" service running on the Worker Node
service Worker {
  // Master calls this to start a container
  rpc StartJob(StartJobRequest) returns (StartJobResponse);

  // Master calls this to kill a container
  rpc StopJob(StopJobRequest) returns (StopJobResponse);
}

message StartJobRequest {
  string job_id = 1;
  string image = 2;       // e.g., "ubuntu:latest"
  repeated string cmd = 3; // e.g., ["echo", "hello"]
}

message StartJobResponse {
  string job_id = 1;
  bool success = 2;
}

message StopJobRequest {
  string job_id = 1;
}

message StopJobResponse {
  bool success = 1;
}
```
- File: `api/proto/v1/worker.proto`

#### Reactoring `worker/main.go`: The Structural Inversion
**Problem**: In Phase 2, our Worker was a **Client-Only** application. It connected to the Master, registered, and then entiered an infinite loop to send Heartbeats. Because the Heartbeat loop blocked the main thread forever, the program **never reached the code** intended to start the gRPC server. When the Master tried to dial the Worker (`StartJob`), it got `connection refused` because port 8080 was never opened.

**Solution**: We moved the client logic (Heartbeats) to the background and made the server logic (listening for commands) the main blocking process.

Before (Blocking Client):
```go
func main() {
    // 1. Register with Master
    // ...

    // 2. Heartbeat Loop (BLOCKING)
    for range ticker.C {
        client.SendHeartbeat(...)
    }

    // 3. Start Server (UNREACHABLE CODE)
    lis, _ := net.Listen("tcp", ":8080")
    grpcServer.Serve(lis) 
}
```
After (Background Client, Blocking Server)
- The heartbeat runs in a ***goroutine***, allowing the main thread to proceed to `Serve()`
```go
func main() {
    // 1. Register with Master
    // ...

    // 2. Heartbeat Loop (NON-BLOCKING)
    // We wrap this in 'go func()' to push it to a background thread
    go func() {
        for range ticker.C {
            client.SendHeartbeat(...)
        }
    }()

    // 3. Start Server (BLOCKING)
    // The main thread now successfully reaches this point and listens for Master commands
    lis, _ := net.Listen("tcp", ":8080")
    grpcServer.Serve(lis) 
}
```

With this, we enable **bi-directional gRPC**. The Worker now simultaneously acts as a **Client** (sending outbound pulses to port 9000) and as a **Server** (receiving inbound commands on port 8080)

#### Add Dispatching in Scheduler Loop



```go
	// scheduler + dispatcher loop
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		for range ticker.C {
			scheduledJobs := clusterManager.Schedule()

			// log changes for debugging
			for _, job := range scheduledJobs {
				log.Printf("Dispatching Job %s to %d nodes...", job.ID, len(job.AssignedNodes))

				// for every node assigned to this job, send a StartJob RPC
				for _, nodeID := range job.AssignedNodes {
					// run dispatch in a goroutine so we don't block the loop
					go func(nID string, j *scheduler.Job) {
						// A. get node info
						node, err := clusterManager.GetNode(nID)
						if err != nil {
							log.Printf("Error retrieving node %s: %v", nID, err)
							return
						}

						// B. construct address (e.g localhost:8080)
						addr := fmt.Sprintf("localhost:%d", node.Port)

						// C. connect to worker
						conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
						if err != nil {
							log.Printf("Failed to connect to worker %s: %v", nID, err)
							return
						}
						defer conn.Close()

						workerClient := pb.NewWorkerClient(conn)

						// D. Send Command
						_, err = workerClient.StartJob(context.Background(), &pb.StartJobRequest{
							JobId: j.ID,
							Image: j.Image,
						})
						if err != nil {
							log.Printf("Failed to start job on worker %s: %v", nID, err)
						} else {
							log.Printf("Worker %s started job %s", nID, j.ID)
						}

					}(nodeID, job)
				}
			}

		}
	}()
```
- File: `cmd/scheduler/main.go`

#### Connecting the Brain to the Muscle (`os/exec`)
Now that the Worker is receiving the `StartJob` command, we need to bridge the gap between Go and RUst. We use Go's `os/exec` packagve to perform a **Fork/Exec** operation, treating the Rust binary as a subprocess
```go
func (s *WorkerServer) StartJob(ctx context.Context, req *pb.StartJobRequest) (*pb.StartJobResponse, error) {
	log.Printf("STARTING CONTAINER: JobID=%s, Image=%s", req.JobId, req.Image)

	// execute the carapace runtime
	// assumes the 'carapace' binary is in the same folder
	cmd := exec.Command("./carapace", "run", req.JobId, req.Image)

	// wire up the output so we see it in this terminal
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// run in background so we don't block the gRPC return
	go func() {
		if err := cmd.Run(); err != nil {
			log.Printf("Job %s failed: %v", req.JobId, err)
		} else {
			log.Printf("Job %s finished successfully", req.JobId)
		}
	}()

	return &pb.StartJobResponse{
		JobId:   req.JobId,
		Success: true,
	}, nil
}
```
- File: `cmd/worker/main.go`

To make this work, namely the `cmd := exec.Command("./carapace", "run", req.JobId, req.Image)` line, the Go worker needs the Rust binary to exist in the same directory, and it needs **Root privelieges** to modify Linux Cgroups. We assume we have the `carapace` repository at the same level as `synapse`

This builds the Rust runtime, copies it and paste in the same directory in `synapse`
```bash
cd ~/github/carapace
cargo build --release
cp target/release/carapace ~/github/synapse/
```

Then we run Worker as **Root**. Compile the worker first
```bash
go build -o worker_bin cmd/worker/main.go
sudo ./worker_bin
```

#### The Big Merge: Deployment and Filesystems

Our custom runtime is not Docker; it does not pull images from the Hub. If we pass `ubuntu:latest`, Carapace will fail with `ENOENT` because it looks for a folder on disk named `ubuntu:latest`. We must create a **valid Root Filesystem (RootFS)** and pass the **absolute path**
```bash
# install docker if haven't already
sudo apt install -y docker.io
# Create a dummy container
sudo docker create --name temp-export ubuntu:latest
# Export the filesystem to a folder
mkdir -p my-rootfs
sudo docker export temp-export | tar -x -C my-rootfs
# Clean up
sudo docker rm temp-export
```
</details>

## Resources
* [Protocol Buffers in Go](https://protobuf.dev/getting-started/gotutorial/)
* [Context Library](https://pkg.go.dev/context)
* [Go routines](https://go.dev/tour/concurrency/1)
* [os/exec](https://pkg.go.dev/os/exec)
	- We use this to allow Go code to act like a human typing commands into a terminal using **Fork/Exec**
	- Fork: The OS creates a clone of the current process (Go worker)
	- Exec: OS replaces that clone's memory with the new program (`./carapace`)
	- Wait: The Go Worker waits for that new process to finish (or runs in the background if we use a goroutine)