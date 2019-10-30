package mhist

import (
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/alexmorten/mhist/models"
)

//Server is the handler for requests
type Server struct {
	store       *Store
	pools       *models.Pools
	grpcHandler *GrpcHandler
	waitGroup   *sync.WaitGroup
}

//ServerConfig ...
type ServerConfig struct {
	GrpcPort   int
	MemorySize int
	DiskSize   int
}

//NewServer returns a new Server
func NewServer(config ServerConfig) *Server {
	pools := models.NewPools()
	diskStore, err := NewDiskStore(pools, config.MemorySize, config.DiskSize)
	if err != nil {
		panic(err)
	}

	store := NewStore(diskStore)

	server := &Server{
		store:     store,
		pools:     pools,
		waitGroup: &sync.WaitGroup{},
	}

	grpcHandler := NewGrpcHandler(server, config.GrpcPort)
	server.grpcHandler = grpcHandler
	store.AddSubscriber(grpcHandler)

	return server
}

//Run the server
func (s *Server) Run() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		signal := <-signals
		log.Printf("received %s, shutting down\n", signal)
		s.Shutdown()
	}()

	s.grpcHandler.Run()
}

//Shutdown all goroutines and connections
func (s *Server) Shutdown() {
	s.grpcHandler.Shutdown()

	s.store.Shutdown()
}
