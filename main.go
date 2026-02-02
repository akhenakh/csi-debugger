package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"sigs.k8s.io/secrets-store-csi-driver/provider/v1alpha1"
)

const (
	appName = "csi-debugger"
)

// Config holds configuration similar to the reference main.go
type Config struct {
	LogLevel   string `env:"LOG_LEVEL" envDefault:"INFO"`
	HTTPPort   int    `env:"HTTP_PORT" envDefault:"8090"`
	SocketPath string `env:"SOCKET_PATH" envDefault:"/tmp/csi-debugger.sock"`
}

// In-Memory Secret Store
type Secret struct {
	Name    string
	Value   string
	Version string
	Mode    int32
}

type MemoryStore struct {
	mu      sync.RWMutex
	secrets map[string]Secret
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		secrets: make(map[string]Secret),
	}
}

func (s *MemoryStore) Set(name, value, version string, mode int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets[name] = Secret{
		Name:    name,
		Value:   value,
		Version: version,
		Mode:    mode,
	}
}

func (s *MemoryStore) Delete(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.secrets, name)
}

func (s *MemoryStore) List() []Secret {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []Secret
	for _, v := range s.secrets {
		list = append(list, v)
	}
	// Sort for stable UI rendering
	sort.Slice(list, func(i, j int) bool {
		return list[i].Name < list[j].Name
	})
	return list
}

func (s *MemoryStore) GetFiles() ([]*v1alpha1.File, []*v1alpha1.ObjectVersion) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var files []*v1alpha1.File
	var versions []*v1alpha1.ObjectVersion

	for _, sec := range s.secrets {
		files = append(files, &v1alpha1.File{
			Path:     sec.Name,
			Mode:     sec.Mode,
			Contents: []byte(sec.Value),
		})
		versions = append(versions, &v1alpha1.ObjectVersion{
			Id:      sec.Name,
			Version: sec.Version,
		})
	}
	return files, versions
}

// gRPC Provider Server (Implements the CSI Driver Provider Interface)
type ProviderServer struct {
	v1alpha1.UnimplementedCSIDriverProviderServer
	store  *MemoryStore
	logger *slog.Logger
}

func (s *ProviderServer) Mount(ctx context.Context, req *v1alpha1.MountRequest) (*v1alpha1.MountResponse, error) {
	s.logger.Info("Mount request received",
		"target_path", req.GetTargetPath(),
		"attributes", req.GetAttributes(),
	)

	// In a real provider, we would parse req.GetAttributes() to know WHICH secrets to fetch.
	// For this debugger, we return everything currently in the MemoryStore to the mount point.
	files, versions := s.store.GetFiles()

	return &v1alpha1.MountResponse{
		Files:         files,
		ObjectVersion: versions,
	}, nil
}

func (s *ProviderServer) Version(ctx context.Context, req *v1alpha1.VersionRequest) (*v1alpha1.VersionResponse, error) {
	s.logger.Info("Version request received", "client_version", req.Version)
	return &v1alpha1.VersionResponse{
		Version:        "v1alpha1",
		RuntimeName:    "csi-debugger-provider",
		RuntimeVersion: "0.0.1",
	}, nil
}

// HTTP Admin Handler

// Embedded simple HTML template for the admin UI
const adminHTML = `
<!DOCTYPE html>
<html>
<head>
    <title>CSI Debugger Admin</title>
    <style>
        body { font-family: sans-serif; max-width: 800px; margin: 0 auto; padding: 20px; }
        table { width: 100%; border-collapse: collapse; margin-top: 20px; }
        th, td { border: 1px solid #ddd; padding: 8px; text-align: left; }
        th { background-color: #f2f2f2; }
        .form-group { margin-bottom: 15px; }
        label { display: block; margin-bottom: 5px; }
        input, textarea { width: 100%; padding: 8px; box-sizing: border-box; }
        button { padding: 10px 15px; background-color: #007bff; color: white; border: none; cursor: pointer; }
        button.delete { background-color: #dc3545; }
        .header { display: flex; justify-content: space-between; align-items: center; }
    </style>
</head>
<body>
    <div class="header">
        <h1>CSI Secret Debugger</h1>
        <button onclick="location.reload()">Refresh</button>
    </div>

    <h3>Active Secrets (In-Memory)</h3>
    <p>These secrets will be returned to the CSI Driver upon the next <code>Mount</code> call.</p>

    <table>
        <thead>
            <tr>
                <th>File Name (Path)</th>
                <th>Content Preview</th>
                <th>Version</th>
                <th>Mode</th>
                <th>Action</th>
            </tr>
        </thead>
        <tbody>
            {{range .}}
            <tr>
                <td>{{.Name}}</td>
                <td>{{.Value}}</td>
                <td>{{.Version}}</td>
                <td>{{.Mode}}</td>
                <td>
                    <form action="/delete" method="POST" style="margin:0;">
                        <input type="hidden" name="name" value="{{.Name}}">
                        <button type="submit" class="delete">Delete</button>
                    </form>
                </td>
            </tr>
            {{else}}
            <tr><td colspan="5">No secrets configured.</td></tr>
            {{end}}
        </tbody>
    </table>

    <hr>

    <h3>Add / Update Secret</h3>
    <form action="/update" method="POST">
        <div class="form-group">
            <label>File Name (e.g., database.yaml)</label>
            <input type="text" name="name" required placeholder="config.json">
        </div>
        <div class="form-group">
            <label>Content</label>
            <textarea name="value" rows="4" required placeholder="super-secret-value"></textarea>
        </div>
        <div class="form-group">
            <label>Version (Arbitrary string, changes trigger rotation)</label>
            <input type="text" name="version" value="v1">
        </div>
        <div class="form-group">
            <label>File Mode (Octal, e.g. 0644)</label>
            <input type="number" name="mode" value="420" placeholder="420 is 0644 decimal">
        </div>
        <button type="submit">Save Secret</button>
    </form>
    
    <hr>
    <h3>Bulk Upload (JSON)</h3>
    <form action="/bulk" method="POST">
        <div class="form-group">
            <label>JSON Array [{"name": "x", "value": "y", "version": "1"}]</label>
            <textarea name="json_data" rows="4"></textarea>
        </div>
        <button type="submit">Upload Bulk</button>
    </form>
</body>
</html>
`

type WebServer struct {
	store  *MemoryStore
	logger *slog.Logger
	tmpl   *template.Template
}

func NewWebServer(logger *slog.Logger, store *MemoryStore) (*WebServer, error) {
	tmpl, err := template.New("index").Parse(adminHTML)
	if err != nil {
		return nil, err
	}
	return &WebServer{store: store, logger: logger, tmpl: tmpl}, nil
}

func (w *WebServer) handleIndex(rw http.ResponseWriter, r *http.Request) {
	secrets := w.store.List()
	if err := w.tmpl.Execute(rw, secrets); err != nil {
		w.logger.Error("failed to render template", "error", err)
		http.Error(rw, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (w *WebServer) handleUpdate(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "Bad request", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	value := r.FormValue("value")
	version := r.FormValue("version")

	// Default mode 0644 (decimal 420)
	mode := int32(420)
	// You could parse r.FormValue("mode") here if you want robust handling

	if name == "" || value == "" {
		http.Error(rw, "Name and Value required", http.StatusBadRequest)
		return
	}

	w.store.Set(name, value, version, mode)
	w.logger.Info("Secret added/updated via UI", "name", name, "version", version)
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

func (w *WebServer) handleDelete(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := r.FormValue("name")
	w.store.Delete(name)
	w.logger.Info("Secret deleted via UI", "name", name)
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

func (w *WebServer) handleBulk(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := r.FormValue("json_data")

	var items []struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Version string `json:"version"`
	}

	if err := json.Unmarshal([]byte(data), &items); err != nil {
		w.logger.Error("Bulk upload failed", "error", err)
		http.Error(rw, "Invalid JSON", http.StatusBadRequest)
		return
	}

	for _, i := range items {
		w.store.Set(i.Name, i.Value, i.Version, 420)
	}

	w.logger.Info("Bulk secrets imported", "count", len(items))
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

func (w *WebServer) RegisterHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/", w.handleIndex)
	mux.HandleFunc("/update", w.handleUpdate)
	mux.HandleFunc("/delete", w.handleDelete)
	mux.HandleFunc("/bulk", w.handleBulk)
}

func main() {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		fmt.Printf("failed to parse config: %+v\n", err)
		os.Exit(1)
	}

	logger := createLogger(cfg, appName)
	slog.SetDefault(logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger.Info("Starting CSI Debugger", "http_port", cfg.HTTPPort, "socket", cfg.SocketPath)

	store := NewMemoryStore()

	// Pre-populate a dummy secret
	store.Set("debug-secret.txt", "Initial value loaded at startup", "v1", 420)

	g, ctx := errgroup.WithContext(ctx)

	// Start gRPC Provider Server (Unix Domain Socket)
	g.Go(func() error {
		return startGRPCServer(ctx, logger, cfg, store)
	})

	// Start HTTP Admin Server
	g.Go(func() error {
		return startHTTPServer(ctx, logger, cfg, store)
	})

	// Handle Signals
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-interrupt:
		logger.Warn("received termination signal, starting graceful shutdown")
		cancel()
	case <-ctx.Done():
		logger.Warn("context cancelled, starting graceful shutdown")
	}

	if err := g.Wait(); err != nil && err != context.Canceled && err != http.ErrServerClosed {
		logger.Error("server group returned an error", "error", err)
		os.Exit(2)
	}
	logger.Info("debugger shut down gracefully")
}

func startGRPCServer(ctx context.Context, logger *slog.Logger, cfg Config, store *MemoryStore) error {
	// Cleanup old socket
	if err := os.Remove(cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0755); err != nil {
		return fmt.Errorf("failed to create socket directory: %w", err)
	}

	lis, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("gRPC server failed to listen on unix socket: %w", err)
	}

	grpcServer := grpc.NewServer()
	providerSrv := &ProviderServer{store: store, logger: logger}

	v1alpha1.RegisterCSIDriverProviderServer(grpcServer, providerSrv)

	// Create a health check function if strictly required by the driver,
	// though usually Version() is enough for the driver's health check.

	logger.Info("gRPC Provider server listening", "address", cfg.SocketPath)

	go func() {
		<-ctx.Done()
		logger.Info("shutting down gRPC server")
		grpcServer.GracefulStop()
	}()

	return grpcServer.Serve(lis)
}

func startHTTPServer(ctx context.Context, logger *slog.Logger, cfg Config, store *MemoryStore) error {
	webServer, err := NewWebServer(logger, store)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	webServer.RegisterHandlers(mux)

	addr := fmt.Sprintf(":%d", cfg.HTTPPort)
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	logger.Info("HTTP Admin server listening", "address", addr)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown error", "error", err)
		}
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func createLogger(cfg Config, appName string) *slog.Logger {
	level := slog.LevelInfo
	if cfg.LogLevel == "DEBUG" {
		level = slog.LevelDebug
	}
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}).WithAttrs([]slog.Attr{slog.String("app", appName)})
	return slog.New(handler)
}
