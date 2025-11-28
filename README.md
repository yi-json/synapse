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
# generating Go structs
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest

# generating gRPC interfaces
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

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
- **Concurrency Safety**: Since thousands of nodes could theoretically join at once, the Scheduler uses a `sync.RMutex`to prevent race conditions during registration


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

### Phase 2: The Worker Node - The Muscle

**Goal:** Establish the **Data Plane**. We need a persistent agent running on every compute node that acts as the "hands" of the system, executing the "brain's" commands.

**What to build:**  
* **The Worker Protocol**: Defing `worker.proto` so the Master knows how to send commands (like `StartJob` to the Worker)
* **Dual-Role Binary**: The Worker is unqiue because it acts as both:
	- Client: connects to the Master to say "I'm alive" (Heartbeats)
	- Server: listens for commands from the Master ("Start Job #50")

**Key Feature:**  
* **Self-Registration Handshake**: Unlike static systems where you have to manually configure a list of IP addresses, Synapse Workers **auto-discover** the Master. A Worker starts up, generates a unique UUID, and announces itself to the cluster dynamically.


#### Success Criteria
- Run `go mod tidy` to install the new UUID and gRPC dependencies.
- Start the Master in Terminal 1 (:9000).
- Start the Worker in Terminal 2 (:8080).
- Verification:
	* Master Log: [GRPC] RegisterWorker: ID=...
	* Worker Log: Registered with Master! Response: Welcome to Synapse


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

### Phase 4: Execution - Carapace Integration

### Phase 5: Topology and Resilience
</details>

## Resources
* [Protocol Buffers in Go](https://protobuf.dev/getting-started/gotutorial/)
* [Context Library](https://pkg.go.dev/context)