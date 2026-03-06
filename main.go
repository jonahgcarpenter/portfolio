/*
Package main implements a lightweight, high-performance backend for an interactive portfolio.
It serves an embedded HTML frontend, dynamically renders Markdown to HTML with live-reloading
via Server-Sent Events (SSE), exposes Prometheus metrics, and continuously polls a Kubernetes
Loki instance to display real-time GitOps activity.
*/
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/yuin/goldmark"
)

//go:embed index.html
var indexHTML []byte

// LokiResponse maps the expected JSON structure returned by the Grafana Loki API.
// We only define the fields we actually need to extract the log streams and values.
type LokiResponse struct {
	Data struct {
		Result []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

var (
	// --- Content Caching ---
	// contentCache stores pre-rendered HTML fragments in memory to serve web requests instantly.
	contentCache = make(map[string][]byte)
	// cacheMutex is a Read/Write lock that prevents race conditions when background workers
	// update the cache while web requests are simultaneously reading from it.
	cacheMutex   sync.RWMutex 
	dataDir      = "./data/"
	
	// --- Server-Sent Events (SSE) Variables ---
	// clients keeps track of all currently connected web browsers.
	clients   = make(map[chan string]bool)
	// broadcast is a channel used to push update notifications to all connected browsers.
	broadcast = make(chan string)
	// sseMutex protects the clients map from concurrent read/writes when users connect/disconnect.
	sseMutex  = sync.Mutex{} 

	// --- Prometheus Metrics Instrumentation ---
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "portfolio_http_requests_total",
			Help: "Total number of HTTP requests for sections",
		},
		[]string{"section"},
	)
	
	activeConnections = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "portfolio_active_sse_connections",
			Help: "Current number of active Server-Sent Event connections",
		},
	)

	markdownRenderDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "portfolio_markdown_render_duration_seconds",
			Help:    "Time taken to render markdown files to HTML",
			Buckets: prometheus.DefBuckets,
		},
	)
)

func main() {
	// Initial render of all local Markdown files into the HTML cache.
	loadAllSections()

	// Start background workers
	go watchDirectory() // Watches local filesystem (or mounted K8s ConfigMap) for edits
	go handleMessages() // Listens on the broadcast channel to push SSE updates
	go watchLokiLogs()  // Starts the background log poller for live GitOps data

	// Register standard web routes
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(indexHTML)
	})
	http.HandleFunc("/api/section/", handleSection)
	http.HandleFunc("/api/events", sseHandler)

	// Register Prometheus Metrics Endpoint
	http.Handle("/metrics", promhttp.Handler())

	log.Println("Server initialized. Listening on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// handleMessages acts as a broker. It listens for section names on the broadcast channel
// and forwards them to every connected client's individual SSE channel.
func handleMessages() {
	for {
		msg := <-broadcast
		sseMutex.Lock()
		for clientChan := range clients {
			clientChan <- msg
		}
		sseMutex.Unlock()
	}
}

// sseHandler upgrades a standard HTTP request to a persistent Server-Sent Events connection.
// This allows the server to push real-time UI updates to the browser without WebSockets.
func sseHandler(w http.ResponseWriter, r *http.Request) {
	// Set headers required for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	clientChan := make(chan string)
	
	// Register the new client safely
	sseMutex.Lock()
	clients[clientChan] = true
	activeConnections.Inc() // Increment active connection gauge for Prometheus
	sseMutex.Unlock()

	// Ensure the client is cleaned up when they close their browser tab
	defer func() {
		sseMutex.Lock()
		delete(clients, clientChan)
		activeConnections.Dec() 
		sseMutex.Unlock()
		close(clientChan)
	}()

	// Keep the connection open indefinitely, flushing data to the browser as it arrives
	for msg := range clientChan {
		fmt.Fprintf(w, "data: %s\n\n", msg)
		w.(http.Flusher).Flush()
	}
}

// watchDirectory monitors the data directory for file changes.
// When deployed to K8s, this detects when FluxCD updates the ConfigMap volume mount.
func watchDirectory() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil { log.Fatal(err) }
	defer watcher.Close()

	watcher.Add(dataDir)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok { return }
			// Only react to Markdown file creations or modifications
			if strings.HasSuffix(event.Name, ".md") && (event.Has(fsnotify.Write) || event.Has(fsnotify.Create)) {
				
				// Clean up the file path to extract just the section name (e.g., "experience")
				parts := strings.Split(strings.TrimSuffix(event.Name, ".md"), "/")
				cleanKey := parts[len(parts)-1]
				cleanKey = strings.TrimPrefix(cleanKey, "data\\") // Handle Windows pathing edge-case

				// Re-render the files and broadcast the update to connected browsers
				loadAllSections()
				broadcast <- cleanKey 
			}
		case err, ok := <-watcher.Errors:
			if !ok { return }
			log.Println("Watcher error:", err)
		}
	}
}

// loadAllSections reads all Markdown files, converts them to HTML, and caches the result.
func loadAllSections() {
	timer := prometheus.NewTimer(markdownRenderDuration) // Start performance timer
	defer timer.ObserveDuration()                        // Record execution time upon exit

	files, _ := os.ReadDir(dataDir)
	if err != nil {
		log.Printf("Could not read directory %s: %v", dataDir, err)
		return
	}

	for _, file := range files {
		log.Printf("Found entry: %s (IsDirectory: %v)", file.Name(), file.IsDir())

		if file.IsDir() || !strings.HasSuffix(file.Name(), ".md") { continue }

		data, _ := os.ReadFile(dataDir + file.Name())
		var buf bytes.Buffer
		goldmark.Convert(data, &buf)

		sectionKey := strings.TrimSuffix(file.Name(), ".md")
		
		// Safely write the rendered HTML into the global content cache
		cacheMutex.Lock()
		contentCache[sectionKey] = buf.Bytes()
		cacheMutex.Unlock()
	}
}

// handleSection serves the pre-rendered HTML for a specific section from memory.
func handleSection(w http.ResponseWriter, r *http.Request) {
	section := strings.TrimPrefix(r.URL.Path, "/api/section/")
	
	// Increment the Prometheus request counter with the requested section name
	httpRequestsTotal.WithLabelValues(section).Inc()

	// Safely read from the global content cache
	cacheMutex.RLock()
	htmlData, exists := contentCache[section]
	cacheMutex.RUnlock()

	if exists {
		w.Header().Set("Content-Type", "text/html")
		w.Write(htmlData)
	} else {
		http.Error(w, "Not found", 404)
	}
}

// watchLokiLogs is a background worker that polls the Loki cluster API at a fixed interval.
// This prevents overwhelming the cluster when multiple users view the portfolio simultaneously.
func watchLokiLogs() {
	// Run once immediately on startup to populate the cache
	updateLokiCache()

	// Tick every 10 seconds
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// If the Loki fetch was successful, notify browsers looking at the "logs" section
		if updateLokiCache() {
			broadcast <- "logs"
		}
	}
}

// updateLokiCache fetches the latest flux-system logs from Loki, parses the JSON payloads,
// sorts them chronologically, and updates the HTML cache.
func updateLokiCache() bool {
	// Allow overriding the internal cluster DNS via environment variable (useful for local port-forwarding)
	lokiURL := os.Getenv("LOKI_URL")
	if lokiURL == "" {
		lokiURL = "http://loki.monitoring.svc.cluster.local:3100"
	}

	query := `{namespace="flux-system"}`
	reqURL := fmt.Sprintf("%s/loki/api/v1/query_range?query=%s&limit=10", lokiURL, url.QueryEscape(query))

	// Enforce a strict timeout so a stalled Loki instance doesn't hang the worker
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(reqURL)

	if err != nil {
		log.Printf("Loki poll failed: %v", err)
		return false
	}
	defer resp.Body.Close()

	var lokiResp LokiResponse
	if err := json.NewDecoder(resp.Body).Decode(&lokiResp); err != nil {
		log.Printf("Loki JSON parse error: %v", err)
		return false
	}

	// Create a slice to temporarily hold logs so they can be sorted later
	type logLine struct {
		timestamp int64
		html      string
	}
	var allLogs []logLine

	// Parse and format the logs
	for _, result := range lokiResp.Data.Result {
		// Extract pod/app labels, defaulting to "flux-system" if missing
		source := result.Stream["app"]
		if source == "" { source = result.Stream["pod"] }
		if source == "" { source = "flux-system" }

		// Strip Kubernetes pod hashes for cleaner UI (e.g., helm-controller-abc-123 -> helm-controller)
		if strings.Contains(source, "-") {
			parts := strings.Split(source, "-")
			if len(parts) >= 3 {
				source = strings.Join(parts[:len(parts)-2], "-")
			}
		}

		for _, val := range result.Values {
			if len(val) == 2 {
				// Parse Loki's raw nanosecond timestamp for sorting
				tsNano, _ := strconv.ParseInt(val[0], 10, 64)
				rawLog := val[1]
				
				var logMap map[string]interface{}
				err := json.Unmarshal([]byte(rawLog), &logMap)
				
				var formatted string
				// If the log is valid JSON and contains a message, format it cleanly
				if err == nil && logMap["msg"] != nil {
					msg := logMap["msg"].(string)
					
					timeStr := ""
					if ts, ok := logMap["ts"].(string); ok {
						timeStr = fmt.Sprintf("<span style=\"color: #8b949e;\">%s</span> ", ts)
					}
					
					// Append context like the GitRepository or HelmChart being reconciled
					contextStr := ""
					if kind, kOk := logMap["controllerKind"].(string); kOk {
						if name, nOk := logMap["name"].(string); nOk {
							contextStr = fmt.Sprintf(" (%s/%s)", kind, name)
						}
					}

					// Color-code log levels (red for errors, default gray for info)
					level := "info"
					if l, ok := logMap["level"].(string); ok { level = l }
					msgColor := "#c9d1d9" 
					if level == "error" { msgColor = "#ff7b72" }

					formatted = fmt.Sprintf("%s<span style=\"color: #7ee787;\">[%s]</span> <span style=\"color: %s;\">%s%s</span>\n", timeStr, source, msgColor, msg, contextStr)
				} else {
					// Fallback formatter for non-JSON or malformed logs
					formatted = fmt.Sprintf("<span style=\"color: #7ee787;\">[%s]</span> %s\n", source, rawLog)
				}

				allLogs = append(allLogs, logLine{timestamp: tsNano, html: formatted})
			}
		}
	}

	// Sort the slice chronologically (oldest to newest)
	sort.Slice(allLogs, func(i, j int) bool {
		return allLogs[i].timestamp < allLogs[j].timestamp
	})

	// Build the final HTML string
	var sb strings.Builder
	sb.WriteString(`<pre style="color: #a5d6ff; font-size: 0.9em; white-space: pre-wrap;">`)
	for _, l := range allLogs {
		sb.WriteString(l.html)
	}
	sb.WriteString(`</pre>`)

	// Safely update the global content cache with the new Loki HTML
	cacheMutex.Lock()
	contentCache["logs"] = []byte(sb.String())
	cacheMutex.Unlock()

	return true
}
