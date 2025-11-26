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
    1. __The Brain: Scheduler__
        * Role: Holds the "state of the world". It knows which nodes are free, which are busy, and where they are physically located
        * Tech: Go, gRPC, Mutexes (for thread safety)
        * Real-World Equivalent: **BorgMaster** or **Kubernetes Scheduler**
    2. __The Muscle: Worker Node__
        * Role: A lightweight agent on every machine. It listens for orders ("Start Job 50") and executes them
        * Tech: Go (agent) calling [Rust Carapace (runtime)](https://github.com/yi-json/carapace)
        * Real-World Equivalent: **Borglet** or **Kubelet**
    3. __THe Interface: CLI__
        * Role: How users submit work
        * Tech: A CLI tool (`synctk submit job.yaml`)
        * Real-World Equivalent: **Kubectl**
    

## Documentation
Documenting all of the steps I've done for my future self.

### Phase 0: Setting up the Environment
Every Go project needs a `go.mod` file that tracks dependencies
    `go mod init github.com/yourusername/myproject`