package rest

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// Server holds the internal state of the REST API server.
type Server struct {
	socketPath string
	state      *state.State
}

// NewServer returns a REST API server object.
func NewServer(_ context.Context, s *state.State, socketPath string) (*Server, error) {
	// Define the struct.
	server := Server{
		socketPath: socketPath,
		state:      s,
	}

	// Create runtime path if missing.
	err := os.Mkdir(filepath.Dir(socketPath), 0o700)
	if err != nil && !os.IsExist(err) {
		return nil, err
	}

	return &server, nil
}

// Serve starts the REST API server.
func (s *Server) Serve(ctx context.Context) error {
	// Setup listener.
	_ = os.Remove(s.socketPath)
	lc := &net.ListenConfig{}

	listener, err := lc.Listen(ctx, "unix", s.socketPath)
	if err != nil {
		return err
	}

	// Setup routing.
	router := http.NewServeMux()

	router.HandleFunc("/", s.apiRoot)
	router.HandleFunc("/1.0", s.apiRoot10)
	router.HandleFunc("/1.0/debug", s.apiDebug)
	router.HandleFunc("/1.0/debug/log", s.apiDebugLog)
	router.HandleFunc("/1.0/services", s.apiServices)
	router.HandleFunc("/1.0/services/{name}", s.apiServicesEndpoint)
	router.HandleFunc("/1.0/system", s.apiSystem)
	router.HandleFunc("/1.0/system/network", s.apiSystemNetwork)
	router.HandleFunc("/1.0/system/security", s.apiSystemSecurity)

	// Setup server.
	server := &http.Server{
		Handler: router,

		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0,
	}

	return server.Serve(listener)
}
