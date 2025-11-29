package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"time"

	// Generate random IDs for the worker
	"github.com/google/uuid"

	// Import our generated Proto definitions
	pb "github.com/yi-json/synapse/api/proto/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	MasterAddr = "localhost:9000"
	WorkerPort = 8080
)

// handles commands from the master
type WorkerServer struct {
	pb.UnimplementedWorkerServer
	WorkerID string
}

// called by the Master when we are assigned a task
func (s *WorkerServer) StartJob(ctx context.Context, req *pb.StartJobRequest) (*pb.StartJobResponse, error) {
	log.Printf("STARTING CONTAINER: JobID=%s, Image=%s", req.JobId, req.Image)

	// execute the carapace runtime
	// assumes the 'carapace' binary is in the same folder
	cmd := exec.Command("./carapace", "run", req.JobId, req.Image, "/bin/sh")

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

// TODO: Stub for now, impl later
func (s *WorkerServer) StopJob(ctx context.Context, req *pb.StopJobRequest) (*pb.StopJobResponse, error) {
	log.Printf("STOPPING JOB: %s", req.JobId)
	return &pb.StopJobResponse{Success: true}, nil
}

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

	// background heartbeat
	// We wrap this in 'go func()' so it runs in the background.
	// This allows the code to continue down to the Server section!
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			_, err := client.SendHeartbeat(context.Background(), &pb.HeartbeatRequest{
				WorkerId: workerID,
			})
			if err != nil {
				log.Printf("Heartbeat failed: %v", err)
			} else {
				log.Printf("Pulse sent")
			}
		}
	}()

	// SERVER MODE: Start Listening for Commands
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", WorkerPort))
	if err != nil {
		log.Fatalf("failed to listen on %d: %v", WorkerPort, err)
	}

	grpcServer := grpc.NewServer()
	// Register THIS code as the handler for Worker commands
	pb.RegisterWorkerServer(grpcServer, &WorkerServer{WorkerID: workerID})

	log.Printf("Worker listening on port %d...", WorkerPort)

	// This blocks forever (keeping the program alive)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
