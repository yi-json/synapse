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
	resp, err := client.SubmitJob(ctx, &pb.SubmitJobRequest{
		Id:        jobID,
		Image:     *image,
		MinCpu:    int32(*cpu),
		MinMemory: int64(*mem * 1024 * 1024), // Convert MB to Bytes
		MinGpu:    int32(*gpu),
	})

	if err != nil {
		log.Fatalf("Submission Failed: %v", err)
	}

	// 4. Print Success
	log.Printf("Master Response: %s (Job ID: %s)", resp.Message, resp.JobId)
}
