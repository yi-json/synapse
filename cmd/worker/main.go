package main

import (
	"context"
	"log"
	"time"

	// Generate random IDs for the worker
	"github.com/google/uuid"

	// Import our generated Proto definitions
	pb "github.com/yi-json/synapse/api/proto/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// where the scheduler (Master) is listening
	MasterAddr = "localhost:9000"

	// the port THIS worker will listen on (we will use this later)
	WorkerPort = 8000
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
		MemoryBytes: 1024 * 1024 * 1024, // 1 GB
		GpuCount:    1,
		Port:        WorkerPort,
	})

	// critical failure check: if we can't join the cluster, we crash
	if err != nil {
		log.Fatalf("could not register: %v", err)
	}

	log.Printf("Success! Master says: %s", response.Message)

	// heartbeat loop
	// 1. create ticker that fires every 5 seconds
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// 2. start the infinite loop
	for range ticker.C {
		// create short timeout for the heartbeat request
		hbCtx, hbCancel := context.WithTimeout(context.Background(), time.Second)

		_, err := client.SendHeartbeat(hbCtx, &pb.HeartbeatRequest{
			WorkerId:    workerID,
			CurrentLoad: 0, // we'll execute real load later
			ActiveJobs:  0,
		})
		hbCancel() // always clean up context

		if err != nil {
			log.Printf("Heartbeat failed: %v", err)
		} else {
			log.Printf("Pulse sent")
		}
	}
}
