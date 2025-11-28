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

// SchedulerServer wraps our internal logic so it can talk gRPC
// it doesn't store state directly; it delegates to the ClusterManager
type SchedulerServer struct {
	pb.UnimplementedSchedulerServer

	// dependency injection: we rely on the interface, not the specific implementation
	cluster scheduler.ClusterManager
}

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
