package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/render"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	proto "github.com/ssengalanto/runic/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

type server struct {
	proto.UnimplementedGreeterServer
	grpc_health_v1.UnimplementedHealthServer
}

func (s *server) Greet(_ context.Context, in *proto.GreetRequest) (*proto.GreetReply, error) {
	return &proto.GreetReply{Message: in.Name + " world"}, nil
}

func (s *server) Check(ctx context.Context, in *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

func (s *server) Watch(in *grpc_health_v1.HealthCheckRequest, _ grpc_health_v1.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "unimplemented")
}

func main() {
	// Create a listener on TCP port
	lis, err := net.Listen("tcp", ":8080")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	// Create a gRPC server object
	s := grpc.NewServer()
	// Attach the Greeter service to the server
	proto.RegisterGreeterServer(s, &server{})
	grpc_health_v1.RegisterHealthServer(s, &server{})

	// Serve gRPC server in a separate goroutine
	log.Println("Serving gRPC on 0.0.0.0:8080")
	go func() {
		if err := s.Serve(lis); err != nil {
			log.Fatalf("Failed to serve gRPC: %v", err)
		}
	}()

	// Create a client connection to the gRPC server we just started
	// This is where the gRPC-Gateway proxies the requests
	conn, err := grpc.DialContext(
		context.Background(),
		"0.0.0.0:8080",
		grpc.WithBlock(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("Failed to dial server: %v", err)
	}

	healthClient := grpc_health_v1.NewHealthClient(conn)
	gwmux := runtime.NewServeMux(runtime.WithHealthzEndpoint(healthClient))

	if err := proto.RegisterGreeterHandler(context.Background(), gwmux, conn); err != nil {
		log.Fatalf("Failed to register gateway: %v", err)
	}

	// Create a new Chi router
	r := chi.NewRouter()

	// Use Chi middleware
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(render.SetContentType(render.ContentTypeJSON))

	// Mount the gRPC HTTP gateway to the root
	r.Mount("/", gwmux)

	dir, _ := os.Getwd()

	swaggerFile := filepath.Join(dir, "swagger", "tmp.swagger.json")
	// mount a path to expose the generated OpenAPI specification on disk
	r.Get("/docs/swagger.json", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, swaggerFile)
	})

	swaggerDir := filepath.Join(dir, "swagger", "ui")
	// mount the Swagger UI that uses the OpenAPI specification path above
	r.Get("/docs/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.StripPrefix("/docs", http.FileServer(http.Dir(swaggerDir))).ServeHTTP(w, r)
	}))

	gwServer := &http.Server{
		Addr:    ":8081",
		Handler: r,
	}

	log.Println("Serving gRPC-Gateway on http://0.0.0.0:8081")

	// Set up a channel to receive signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Run the server in a goroutine
	go func() {
		if err := gwServer.ListenAndServe(); err != nil {
			log.Fatalf("Failed to serve gRPC-Gateway: %v", err)
		}
	}()

	// Block until a signal is received
	<-stop
	log.Println("Shutting down server...")

	// Create a context with a timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Shutdown the gRPC-Gateway server
	if err := gwServer.Shutdown(ctx); err != nil {
		log.Printf("Error during server shutdown: %v", err)
	}

	// Shutdown the gRPC server
	s.GracefulStop()
	log.Println("Server gracefully stopped")
}
