package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// TaskStatus represents the status of a download task
type TaskStatus struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"` // "single" or "list"
	VideoID      string     `json:"video_id"`
	ListID       string     `json:"list_id,omitempty"`
	Status       string     `json:"status"` // "pending", "downloading", "completed", "failed"
	Title        string     `json:"title,omitempty"`
	Progress     int        `json:"progress"` // 0-100
	TotalSize    int64      `json:"total_size,omitempty"`
	Downloaded   int64      `json:"downloaded,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// WebServer manages the web interface
type WebServer struct {
	server     *http.Server
	tasks      map[string]*TaskStatus
	tasksMutex sync.RWMutex
}

// NewWebServer creates a new web server instance
func NewWebServer() *WebServer {
	return &WebServer{
		tasks: make(map[string]*TaskStatus),
	}
}

// AddTask adds a new task to the server
func (ws *WebServer) AddTask(task *TaskStatus) {
	ws.tasksMutex.Lock()
	defer ws.tasksMutex.Unlock()
	ws.tasks[task.ID] = task
}

// GetTask returns a task by ID
func (ws *WebServer) GetTask(id string) *TaskStatus {
	ws.tasksMutex.RLock()
	defer ws.tasksMutex.RUnlock()
	return ws.tasks[id]
}

// GetAllTasks returns all tasks
func (ws *WebServer) GetAllTasks() []*TaskStatus {
	ws.tasksMutex.RLock()
	defer ws.tasksMutex.RUnlock()
	tasks := make([]*TaskStatus, 0, len(ws.tasks))
	for _, task := range ws.tasks {
		tasks = append(tasks, task)
	}
	return tasks
}

// UpdateTask updates a task's status
func (ws *WebServer) UpdateTask(id string, updates map[string]interface{}) {
	ws.tasksMutex.Lock()
	defer ws.tasksMutex.Unlock()
	if task, ok := ws.tasks[id]; ok {
		for key, value := range updates {
			switch key {
			case "status":
				task.Status = value.(string)
			case "progress":
				task.Progress = int(value.(float64))
			case "title":
				task.Title = value.(string)
			case "total_size":
				task.TotalSize = int64(value.(float64))
			case "downloaded":
				task.Downloaded = int64(value.(float64))
			case "error_message":
				task.ErrorMessage = value.(string)
			case "completed_at":
				t := time.Now()
				task.CompletedAt = &t
			}
		}
	}
}

// SetupRoutes sets up the HTTP routes for the web server
func (ws *WebServer) SetupRoutes() {
	http.HandleFunc("/api/tasks", ws.handleTasks)
	http.HandleFunc("/api/tasks/", ws.handleTaskByID)
	http.HandleFunc("/api/submit", ws.handleSubmit)
	http.HandleFunc("/api/status/", ws.handleStatus)
	http.HandleFunc("/api/progress", ws.handleProgress)
	http.Handle("/", http.FileServer(http.Dir("./web")))
}

func (ws *WebServer) handleTasks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	switch r.Method {
	case "GET":
		tasks := ws.GetAllTasks()
		json.NewEncoder(w).Encode(tasks)
	case "POST":
		var taskReq struct {
			VideoID string `json:"video_id"`
			ListID  string `json:"list_id"`
			Type    string `json:"type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&taskReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		task := &TaskStatus{
			ID:        fmt.Sprintf("%s_%d", taskReq.VideoID, time.Now().UnixNano()),
			Type:      taskReq.Type,
			VideoID:   taskReq.VideoID,
			ListID:    taskReq.ListID,
			Status:    "pending",
			CreatedAt: time.Now(),
		}
		ws.AddTask(task)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(task)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (ws *WebServer) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	id := r.URL.Path[len("/api/tasks/"):]

	task := ws.GetTask(id)
	if task == nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(task)
}

func (ws *WebServer) handleSubmit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		VideoID string `json:"video_id"`
		ListID  string `json:"list_id"`
		Mode    string `json:"mode"` // "single" or "list"
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	task := &TaskStatus{
		ID:        fmt.Sprintf("%s_%d", req.VideoID, time.Now().UnixNano()),
		Type:      req.Mode,
		VideoID:   req.VideoID,
		ListID:    req.ListID,
		Status:    "pending",
		CreatedAt: time.Now(),
	}

	ws.AddTask(task)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(task)
}

func (ws *WebServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	id := r.URL.Path[len("/api/status/"):]

	task := ws.GetTask(id)
	if task == nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(task)
}

func (ws *WebServer) handleProgress(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Return all tasks with their current progress
	tasks := ws.GetAllTasks()
	json.NewEncoder(w).Encode(tasks)
}

// Start starts the web server
func (ws *WebServer) Start(addr string) error {
	ws.SetupRoutes()

	ws.server = &http.Server{
		Addr:         addr,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	displayAddr := addr
	if strings.HasPrefix(displayAddr, ":") {
		displayAddr = "localhost" + displayAddr
	}

	log.Printf("Web server starting on %s", addr)
	log.Printf("Visit http://%s to access the web interface", displayAddr)
	log.Printf("Press Ctrl+C to stop the server")

	return ws.server.ListenAndServe()
}

// Stop stops the web server
func (ws *WebServer) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return ws.server.Shutdown(ctx)
}

// RunWebServer starts the web interface server
func RunWebServer(addr, cacheDir, downDir string, maxWorkers int) {
	// Ensure directories exist
	if cacheDir == "" {
		cacheDir = "./cache"
	}
	if downDir == "" {
		downDir = "./download"
	}
	os.MkdirAll(cacheDir, 0755)
	os.MkdirAll(downDir, 0755)

	if maxWorkers <= 0 {
		maxWorkers = 3
	}

	log.Printf("Web server mode enabled")
	log.Printf("Configuration: CacheDir=%s, DownDir=%s, Workers=%d",
		cacheDir, downDir, maxWorkers)

	// Create web server instance
	webSrv := NewWebServer()

	// Start web server
	if err := webSrv.Start(addr); err != nil {
		log.Fatalf("Web server failed: %v", err)
	}
}
