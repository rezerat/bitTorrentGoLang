package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

//go:embed ui/index.html
var uiAssets embed.FS

type AppServer struct {
	mu          sync.RWMutex
	sessions    map[string]*DownloadSession
	order       []string
	defaultOut  string
	activeDemos []*LocalSwarmDemo
}

func NewAppServer(defaultOut string) *AppServer {
	_ = os.MkdirAll(defaultOut, 0755)
	return &AppServer{
		sessions:   make(map[string]*DownloadSession),
		defaultOut: defaultOut,
	}
}

type sessionDTO struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	InfoHash      string      `json:"infoHash"`
	Announce      string      `json:"announce"`
	Trackers      []string    `json:"trackers"`
	TotalSize     int64       `json:"totalSize"`
	PieceLength   int         `json:"pieceLength"`
	TotalPieces   int         `json:"totalPieces"`
	CompletedPcs  int32       `json:"completedPieces"`
	Downloaded    int64       `json:"downloaded"`
	SpeedBps      int64       `json:"speedBps"`
	Status        string      `json:"status"`
	ErrorMsg      string      `json:"errorMsg"`
	OutputDir     string      `json:"outputDir"`
	ActivePeers   int32       `json:"activePeers"`
	TotalPeers    int         `json:"totalPeers"`
	PeersList     []string    `json:"peersList"`
	PieceStatuses []int       `json:"pieceStatuses"`
	Files         []FileEntry `json:"files"`
	Logs          []string    `json:"logs"`
}

func (s *AppServer) toDTO(sess *DownloadSession) sessionDTO {
	sess.mu.RLock()
	defer sess.mu.RUnlock()

	peersStr := make([]string, len(sess.Peers))
	for i, p := range sess.Peers {
		peersStr[i] = p.String()
	}

	piecesCopy := make([]int, len(sess.PieceStatuses))
	for i, st := range sess.PieceStatuses {
		piecesCopy[i] = int(st)
	}

	logsCopy := make([]string, len(sess.Logs))
	copy(logsCopy, sess.Logs)

	return sessionDTO{
		ID:            sess.ID,
		Name:          sess.Torrent.Name,
		InfoHash:      sess.Torrent.InfoHashHex(),
		Announce:      sess.Torrent.Announce,
		Trackers:      sess.Torrent.Trackers,
		TotalSize:     sess.Torrent.Length,
		PieceLength:   sess.Torrent.PieceLength,
		TotalPieces:   len(sess.Torrent.PieceHashes),
		CompletedPcs:  atomic.LoadInt32(&sess.CompletedPcs),
		Downloaded:    atomic.LoadInt64(&sess.Downloaded),
		SpeedBps:      atomic.LoadInt64(&sess.SpeedBps),
		Status:        sess.Status,
		ErrorMsg:      sess.ErrorMsg,
		OutputDir:     sess.OutputDir,
		ActivePeers:   atomic.LoadInt32(&sess.ActivePeers),
		TotalPeers:    len(sess.Peers),
		PeersList:     peersStr,
		PieceStatuses: piecesCopy,
		Files:         sess.Torrent.Files,
		Logs:          logsCopy,
	}
}

func (s *AppServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := uiAssets.ReadFile("ui/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/api/torrents", s.handleTorrents)
	mux.HandleFunc("/api/torrents/upload", s.handleUploadTorrent)
	mux.HandleFunc("/api/torrents/demo", s.handleStartDemo)
	mux.HandleFunc("/api/torrents/action", s.handleTorrentAction)
}

func (s *AppServer) handleTorrents(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	list := make([]sessionDTO, 0, len(s.order))
	for _, id := range s.order {
		if sess, ok := s.sessions[id]; ok {
			list = append(list, s.toDTO(sess))
		}
	}
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"defaultOut": s.defaultOut,
		"torrents":   list,
	})
}

func (s *AppServer) handleUploadTorrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonError(w, "failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("torrent")
	if err != nil {
		jsonError(w, "missing torrent file: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	rawBytes, err := io.ReadAll(file)
	if err != nil {
		jsonError(w, "failed to read file: "+err.Error(), http.StatusBadRequest)
		return
	}

	tf, err := parseTorrentBytes(rawBytes)
	if err != nil {
		jsonError(w, "invalid .torrent file: "+err.Error(), http.StatusBadRequest)
		return
	}

	outDir := strings.TrimSpace(r.FormValue("outputDir"))
	if outDir == "" {
		outDir = s.defaultOut
	}

	customPeers := parseCustomPeers(r.FormValue("customPeers"))
	sess := s.addAndStartSession(tf, outDir, customPeers)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.toDTO(sess))
}

func (s *AppServer) handleStartDemo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	demo, err := NewLocalSwarmDemo()
	if err != nil {
		jsonError(w, "failed to start local swarm: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.activeDemos = append(s.activeDemos, demo)
	s.mu.Unlock()

	demoTorrentPath := filepath.Join(s.defaultOut, "data.torrent")
	_ = os.WriteFile(demoTorrentPath, demo.TorrentBytes, 0644)
	_ = os.WriteFile("data.torrent", demo.TorrentBytes, 0644)

	sess := s.addAndStartSession(demo.TorrentFile, s.defaultOut, nil)
	sess.AddLog("Local Swarm Testbed active: HTTP Tracker at %s with %d TCP seeders", demo.TrackerURL, len(demo.SeederAddrs))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.toDTO(sess))
}

func (s *AppServer) handleTorrentAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	sess, ok := s.sessions[req.ID]
	if !ok {
		s.mu.Unlock()
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}

	if req.Action == "remove" {
		delete(s.sessions, req.ID)
		newOrder := make([]string, 0, len(s.order))
		for _, id := range s.order {
			if id != req.ID {
				newOrder = append(newOrder, id)
			}
		}
		s.order = newOrder
		s.mu.Unlock()
		sess.Stop()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "removed"})
		return
	}
	s.mu.Unlock()

	switch req.Action {
	case "pause":
		newStatus := sess.TogglePause()
		sess.AddLog("Session state changed to %s", newStatus)
	case "stop":
		sess.Stop()
		sess.AddLog("Session stopped by user")
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.toDTO(sess))
}

func (s *AppServer) addAndStartSession(tf *TorrentFile, outDir string, customPeers []Peer) *DownloadSession {
	s.mu.Lock()
	if existing, ok := s.sessions[tf.InfoHashHex()]; ok {
		existing.Stop()
	} else {
		s.order = append([]string{tf.InfoHashHex()}, s.order...)
	}
	sess := NewDownloadSession(tf, outDir)
	s.sessions[sess.ID] = sess
	s.mu.Unlock()

	sess.Start(customPeers)
	return sess
}

func parseCustomPeers(raw string) []Peer {
	var peers []Peer
	for _, token := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == ' ' || r == ';'
	}) {
		host, portStr, err := net.SplitHostPort(strings.TrimSpace(token))
		if err != nil {
			continue
		}
		ip := net.ParseIP(host)
		port, err := strconv.Atoi(portStr)
		if ip == nil || err != nil || port <= 0 || port > 65535 {
			continue
		}
		peers = append(peers, Peer{IP: ip, Port: uint16(port)})
	}
	return peers
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	}
	if cmd != nil {
		_ = cmd.Start()
	}
}

func main() {
	cliTorrent := flag.String("torrent", "", "Path to a .torrent file to download in CLI mode")
	cliOut := flag.String("out", "", "Output path for CLI download or default directory for GUI")
	addr := flag.String("addr", "127.0.0.1:8080", "Address for the GUI server")
	noBrowser := flag.Bool("no-browser", false, "Do not automatically open the browser when starting GUI")
	flag.Parse()

	defaultDownloads := *cliOut
	if defaultDownloads == "" {
		cwd, _ := os.Getwd()
		defaultDownloads = filepath.Join(cwd, "downloads")
	}

	if *cliTorrent != "" {
		raw, err := os.ReadFile(*cliTorrent)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open torrent file: %v\n", err)
			os.Exit(1)
		}
		tf, err := parseTorrentBytes(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to parse torrent: %v\n", err)
			os.Exit(1)
		}
		target := *cliOut
		if target == "" {
			target = tf.Name
		}
		fmt.Printf("Downloading %s (%d bytes, %d pieces)...\n", tf.Name, tf.Length, len(tf.PieceHashes))
		if err := tf.DownloadToFile(target); err != nil {
			fmt.Fprintf(os.Stderr, "Download error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Saved successfully to %s\n", target)
		return
	}

	app := NewAppServer(defaultDownloads)

	if raw, err := os.ReadFile("data.torrent"); err == nil {
		if tf, err := parseTorrentBytes(raw); err == nil && !strings.HasPrefix(tf.Announce, "http://127.0.0.1:") {
			app.addAndStartSession(tf, defaultDownloads, nil)
		}
	}

	mux := http.NewServeMux()
	app.registerRoutes(mux)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to bind server port: %v\n", err)
			os.Exit(1)
		}
	}

	uiURL := fmt.Sprintf("http://%s", ln.Addr().String())
	fmt.Printf("BitTorrent GUI Client running at %s\n", uiURL)
	fmt.Printf("Downloads directory: %s\n", defaultDownloads)

	if !*noBrowser {
		go openBrowser(uiURL)
	}

	if err := http.Serve(ln, mux); err != nil {
		fmt.Fprintf(os.Stderr, "Server closed: %v\n", err)
	}
}
