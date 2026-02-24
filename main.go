package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/events"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	waProto "go.mau.fi/whatsmeow/binary/proto"
)

// Global variables
var (
	db              *sql.DB
	client          *whatsmeow.Client
	logger          waLog.Logger
	appConfig       *AutoConfig
	configMutex     sync.RWMutex
	linkedMutex     sync.RWMutex
	isLinked        bool
	pairingCode     string
	pairingMutex    sync.RWMutex
	wsHub           *WebSocketHub
	connectionState *ConnectionState
	configCache     *ConfigCache
)

// AutoConfig holds application configuration
type AutoConfig struct {
	Enabled       bool      `json:"enabled"`
	Numbers       []string  `json:"numbers"`
	Message       string    `json:"message"`
	ReplyEnable   bool      `json:"reply_enable"`
	ReplyText     string    `json:"reply_text"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ConnectionState tracks connection info
type ConnectionState struct {
	mu        sync.RWMutex
	Connected bool
	JID       string
	LinkedAt  time.Time
	LastCheck time.Time
}

// ConfigCache provides cached config access
type ConfigCache struct {
	mu        sync.RWMutex
	config    *AutoConfig
	expiresAt time.Time
}

// APIResponse is a generic response structure
type APIResponse struct {
	Status  string      `json:"status"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
	Code    int         `json:"code,omitempty"`
}

// ConnectionInfo provides device connection status
type ConnectionInfo struct {
	Connected bool      `json:"connected"`
	JID       string    `json:"jid,omitempty"`
	LinkedAt  time.Time `json:"linked_at,omitempty"`
	LastCheck time.Time `json:"last_check,omitempty"`
}

// WebSocketHub manages WebSocket connections
type WebSocketHub struct {
	clients    map[*WebSocketClient]bool
	broadcast  chan interface{}
	register   chan *WebSocketClient
	unregister chan *WebSocketClient
	mu         sync.RWMutex
}

// WebSocketClient represents a connected client
type WebSocketClient struct {
	hub  *WebSocketHub
	conn *websocket.Conn
	send chan interface{}
}

// WebSocketEvent represents an event sent over WebSocket
type WebSocketEvent struct {
	Type      string      `json:"type"`
	Data      interface{} `json:"data,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
}

func init() {
	logger = waLog.NewWithLogger("whatsmeow", log.New(os.Stdout, "[WA] ", log.LstdFlags|log.Lshortfile))
	wsHub = newWebSocketHub()
	connectionState = &ConnectionState{}
	configCache = &ConfigCache{}
}

func main() {
	// Initialize config
	appConfig = &AutoConfig{
		Enabled:     false,
		Numbers:     []string{},
		Message:     "Welcome to our WhatsApp service!",
		ReplyEnable: false,
		ReplyText:   "Thanks for your message!",
		UpdatedAt:   time.Now(),
	}

	loadConfigFromFile()

	// Database
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("❌ DATABASE_URL environment variable not set")
	}

	var err error
	db, err = initializeDatabase(dbURL)
	if err != nil {
		log.Fatalf("❌ Failed to initialize database: %v", err)
	}
	defer db.Close()
	log.Println("✅ Database connected successfully")

	// WhatsApp Client
	client, err = initializeWhatsAppClient(db)
	if err != nil {
		log.Fatalf("❌ Failed to initialize WhatsApp client: %v", err)
	}
	log.Println("✅ WhatsApp client initialized")

	// Event handlers
	client.AddEventHandler(handleWhatsAppEvent)

	// Initialize bulk messaging engine
	initBulkMessagingEngine()

	// Start WebSocket hub
	go wsHub.run()

	// Setup HTTP routes
	setupRoutes()
	setupBulkMessagingRoutes()

	// Start HTTP server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	server := &http.Server{
		Addr:           ":" + port,
		Handler:        http.DefaultServeMux,
		ReadTimeout:    15 * time.Second,
		WriteTimeout:   15 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	log.Printf("🚀 Server starting on http://localhost:%s", port)

	done := make(chan struct{})
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		log.Println("\n⏹️  Shutdown signal received, gracefully stopping...")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			log.Printf("❌ Server shutdown error: %v", err)
		}

		if client != nil && client.IsConnected() {
			client.Disconnect()
			log.Println("✅ WhatsApp client disconnected")
		}

		if bulkEngine != nil {
			bulkEngine.Stop()
		}

		wsHub.close()
		close(done)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("❌ Server error: %v", err)
	}

	<-done
	log.Println("✅ Server stopped cleanly")
}

// initializeDatabase creates a database connection pool
func initializeDatabase(dbURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(10 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("database ping failed: %w", err)
	}

	if err := runMigrations(db); err != nil {
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	return db, nil
}

// runMigrations ensures all required tables exist
func runMigrations(db *sql.DB) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS automation_config (
			id SERIAL PRIMARY KEY,
			phone_number VARCHAR(20) UNIQUE,
			auto_reply_enabled BOOLEAN DEFAULT FALSE,
			auto_reply_message TEXT,
			welcome_message TEXT,
			welcome_numbers TEXT,
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		);`,
		`CREATE TABLE IF NOT EXISTS connection_status (
			id SERIAL PRIMARY KEY,
			phone_number VARCHAR(20) UNIQUE,
			is_connected BOOLEAN DEFAULT FALSE,
			paired_at TIMESTAMP,
			last_active_at TIMESTAMP,
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		);`,
		`CREATE TABLE IF NOT EXISTS message_logs (
			id SERIAL PRIMARY KEY,
			phone_number VARCHAR(20),
			recipient VARCHAR(20),
			message TEXT,
			status VARCHAR(20),
			created_at TIMESTAMP DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS idx_message_logs_created ON message_logs(created_at);`,
	}

	for _, migration := range migrations {
		if _, err := db.Exec(migration); err != nil {
			return fmt.Errorf("migration error: %w", err)
		}
	}

	log.Println("✅ Database migrations completed")
	return nil
}

// initializeWhatsAppClient sets up the Whatsmeow client
func initializeWhatsAppClient(db *sql.DB) (*whatsmeow.Client, error) {
	container, err := sqlstore.New("postgres", db, "", logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create sqlstore: %w", err)
	}

	device, err := container.GetFirstDevice()
	if err != nil {
		return nil, fmt.Errorf("failed to get device: %w", err)
	}

	if device == nil {
		log.Println("📱 No existing device found, creating new one...")
		device = container.NewDevice()
	} else {
		linkedMutex.Lock()
		isLinked = true
		linkedMutex.Unlock()
		log.Println("📱 Existing device found, resuming connection...")
	}

	client := whatsmeow.NewClient(device, logger)

	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("failed to connect to WhatsApp: %w", err)
	}

	log.Println("✅ WhatsApp client connected")
	return client, nil
}

// setupRoutes defines all HTTP endpoints
func setupRoutes() {
	http.HandleFunc("/", serveIndex)
	http.HandleFunc("/admin", serveAdmin)
	http.HandleFunc("/pair", handlePair)
	http.HandleFunc("/is-linked", isLinkedHandler)
	http.HandleFunc("/api/info", getConnectionInfo)
	http.HandleFunc("/api/config", handleConfig)
	http.HandleFunc("/send", sendMessage)
	http.HandleFunc("/logout", handleLogout)
	http.HandleFunc("/health", healthCheck)
	http.HandleFunc("/ws", handleWebSocket)
	log.Println("✅ Routes registered")
}

// handleWhatsAppEvent processes WhatsApp events
func handleWhatsAppEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.PairSuccess:
		log.Printf("✅ Device paired successfully: %v", v.ID)
		linkedMutex.Lock()
		isLinked = true
		linkedMutex.Unlock()
		updateConnectionState(true)
		wsHub.broadcast <- WebSocketEvent{
			Type: "pair_success",
			Data: map[string]interface{}{
				"id": v.ID.String(),
			},
			Timestamp: time.Now(),
		}
		go handlePairSuccess(v)

	case *events.Message:
		go handleIncomingMessage(v)

	case *events.Connected:
		log.Println("🟢 Connected to WhatsApp")
		updateConnectionState(true)
		wsHub.broadcast <- WebSocketEvent{
			Type:      "connection_changed",
			Data:      map[string]bool{"connected": true},
			Timestamp: time.Now(),
		}

	case *events.Disconnected:
		log.Println("🔴 Disconnected from WhatsApp")
		updateConnectionState(false)
		wsHub.broadcast <- WebSocketEvent{
			Type:      "connection_changed",
			Data:      map[string]bool{"connected": false},
			Timestamp: time.Now(),
		}

	case *events.KeepAliveTimeout:
		log.Println("⚠️  KeepAlive timeout, attempting reconnect...")
		if !client.IsConnected() {
			retryWithBackoff(func() error {
				return client.Connect()
			}, 3, 1*time.Second)
		}
	}
}

// HTTP Handlers

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	http.ServeFile(w, r, "index.html")
}

func serveAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	http.ServeFile(w, r, "admin.html")
}

func handlePair(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if client == nil || !client.IsConnected() {
		respondWithError(w, http.StatusServiceUnavailable, "WhatsApp client not connected")
		return
	}

	linkedMutex.RLock()
	if isLinked {
		linkedMutex.RUnlock()
		respondWithError(w, http.StatusBadRequest, "Device already linked")
		return
	}
	linkedMutex.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code, err := client.GetPairingCode(ctx)
	if err != nil {
		log.Printf("❌ Error getting pairing code: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to generate pairing code")
		return
	}

	pairingMutex.Lock()
	pairingCode = code
	pairingMutex.Unlock()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"pairing_code": code,
	})
}

func isLinkedHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	linkedMutex.RLock()
	linked := isLinked
	linkedMutex.RUnlock()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{
		"linked": linked,
	})
}

func getConnectionInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if client == nil {
		respondWithError(w, http.StatusServiceUnavailable, "Client not initialized")
		return
	}

	linkedMutex.RLock()
	linked := isLinked
	linkedMutex.RUnlock()

	connectionState.mu.RLock()
	info := ConnectionInfo{
		Connected: client.IsConnected() && linked,
		JID:       connectionState.JID,
		LinkedAt:  connectionState.LinkedAt,
		LastCheck: time.Now(),
	}
	connectionState.mu.RUnlock()

	if client.Store.ID != nil {
		info.JID = client.Store.ID.String()
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(info)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodGet {
		configMutex.RLock()
		config := appConfig
		configMutex.RUnlock()

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(config)

	} else if r.Method == http.MethodPost {
		var newConfig AutoConfig
		body, err := io.ReadAll(r.Body)
		if err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid body")
			return
		}
		defer r.Body.Close()

		if err := json.Unmarshal(body, &newConfig); err != nil {
			respondWithError(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		if len(newConfig.Message) == 0 {
			newConfig.Message = "Welcome to our WhatsApp service!"
		}
		if len(newConfig.ReplyText) == 0 {
			newConfig.ReplyText = "Thanks for your message!"
		}

		newConfig.UpdatedAt = time.Now()

		configMutex.Lock()
		appConfig = &newConfig
		configMutex.Unlock()

		configCache.invalidate()

		if err := saveConfigToFile(newConfig); err != nil {
			log.Printf("⚠️  Failed to save config to file: %v", err)
		}

		wsHub.broadcast <- WebSocketEvent{
			Type:      "config_updated",
			Data:      newConfig,
			Timestamp: time.Now(),
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(APIResponse{
			Status:  "success",
			Message: "Configuration updated",
			Data:    newConfig,
		})
	} else {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func sendMessage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		PhoneNumber string `json:"phone_number"`
		Message     string `json:"message"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request")
		return
	}

	if len(strings.TrimSpace(req.PhoneNumber)) == 0 {
		respondWithError(w, http.StatusBadRequest, "Phone number is required")
		return
	}
	if len(strings.TrimSpace(req.Message)) == 0 {
		respondWithError(w, http.StatusBadRequest, "Message is required")
		return
	}
	if len(req.Message) > 4096 {
		respondWithError(w, http.StatusBadRequest, "Message too long (max 4096 chars)")
		return
	}

	if client == nil || !client.IsConnected() {
		respondWithError(w, http.StatusServiceUnavailable, "Not connected to WhatsApp")
		return
	}

	jid := formatPhoneNumber(req.PhoneNumber)

	msg := &waProto.Message{
		Conversation: proto.String(req.Message),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := client.SendMessage(ctx, jid, msg)
	if err != nil {
		log.Printf("❌ Error sending message: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to send message")
		return
	}

	logMessage(req.PhoneNumber, req.Message, "sent")

	wsHub.broadcast <- WebSocketEvent{
		Type: "message_sent",
		Data: map[string]interface{}{
			"phone": req.PhoneNumber,
			"text":  req.Message,
		},
		Timestamp: time.Now(),
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(APIResponse{
		Status:  "success",
		Message: "Message sent successfully",
	})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if client == nil {
		respondWithError(w, http.StatusBadRequest, "Not logged in")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Logout(); err != nil {
		log.Printf("❌ Error during logout: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Logout failed")
		return
	}

	linkedMutex.Lock()
	isLinked = false
	linkedMutex.Unlock()

	updateConnectionState(false)

	wsHub.broadcast <- WebSocketEvent{
		Type:      "logged_out",
		Timestamp: time.Now(),
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(APIResponse{
		Status:  "success",
		Message: "Logged out successfully",
	})
}

func healthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
	})
}

// WebSocket Handler
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	client := &WebSocketClient{
		hub:  wsHub,
		conn: conn,
		send: make(chan interface{}, 256),
	}

	wsHub.register <- client

	go client.writePump()
	go client.readPump()
}

// Helper Functions

func handlePairSuccess(evt *events.PairSuccess) {
	log.Println("⏳ Waiting 5 seconds before sending welcome messages...")
	time.Sleep(5 * time.Second)

	configMutex.RLock()
	if !appConfig.Enabled || len(appConfig.Numbers) == 0 {
		configMutex.RUnlock()
		log.Println("⚠️  Auto-sender not enabled or no numbers configured")
		return
	}

	numbers := appConfig.Numbers
	message := appConfig.Message
	configMutex.RUnlock()

	log.Printf("📤 Sending welcome messages to %d numbers...", len(numbers))

	for _, phoneNumber := range numbers {
		time.Sleep(2 * time.Second)

		if client == nil || !client.IsConnected() {
			log.Printf("⚠️  Client not connected, skipping message to %s", phoneNumber)
			continue
		}

		jid := formatPhoneNumber(phoneNumber)

		msg := &waProto.Message{
			Conversation: proto.String(message),
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, err := client.SendMessage(ctx, jid, msg)
		cancel()

		if err != nil {
			log.Printf("❌ Failed to send message to %s: %v", phoneNumber, err)
		} else {
			log.Printf("✅ Message sent to %s", phoneNumber)
			logMessage(phoneNumber, message, "auto_sent")
		}
	}

	log.Println("✅ Welcome messages sending completed")
}

func handleIncomingMessage(evt *events.Message) {
	if evt.Info.IsGroup || evt.Info.FromMe {
		return
	}

	configMutex.RLock()
	if !appConfig.ReplyEnable {
		configMutex.RUnlock()
		return
	}
	replyText := appConfig.ReplyText
	configMutex.RUnlock()

	if client == nil || !client.IsConnected() {
		return
	}

	var msgText string
	if evt.Message.Conversation != nil {
		msgText = *evt.Message.Conversation
	} else if evt.Message.ExtendedTextMessage != nil && evt.Message.ExtendedTextMessage.Text != nil {
		msgText = *evt.Message.ExtendedTextMessage.Text
	} else {
		return
	}

	log.Printf("📨 Auto-replying to %s", evt.Info.Sender)

	msg := &waProto.Message{
		Conversation: proto.String(replyText),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	_, err := client.SendMessage(ctx, evt.Info.Sender, msg)
	cancel()

	if err != nil {
		log.Printf("❌ Failed to send auto-reply: %v", err)
	} else {
		log.Printf("✅ Auto-reply sent to %s", evt.Info.Sender)
		logMessage(evt.Info.Sender.String(), replyText, "auto_reply")
	}
}

func formatPhoneNumber(phoneNumber string) types.JID {
	cleaned := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, phoneNumber)

	if !strings.HasPrefix(cleaned, "91") && len(cleaned) == 10 {
		cleaned = "91" + cleaned
	}

	return types.NewJID(cleaned, types.DefaultUserServer)
}

func saveConfigToFile(config AutoConfig) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile("config.json", data, 0644)
}

func loadConfigFromFile() {
	data, err := os.ReadFile("config.json")
	if err != nil {
		log.Println("⚠️  No config.json found, using defaults")
		return
	}

	var config AutoConfig
	if err := json.Unmarshal(data, &config); err != nil {
		log.Printf("⚠️  Failed to parse config.json: %v", err)
		return
	}

	configMutex.Lock()
	appConfig = &config
	configMutex.Unlock()

	log.Println("✅ Config loaded from config.json")
}

func updateConnectionState(connected bool) {
	connectionState.mu.Lock()
	defer connectionState.mu.Unlock()

	connectionState.Connected = connected
	connectionState.LastCheck = time.Now()
	if connected && connectionState.LinkedAt.IsZero() {
		connectionState.LinkedAt = time.Now()
	}
}

func logMessage(phone, message, status string) {
	go func() {
		_, err := db.Exec(
			"INSERT INTO message_logs (phone_number, recipient, message, status) VALUES ($1, $2, $3, $4)",
			"", phone, message, status,
		)
		if err != nil {
			log.Printf("Failed to log message: %v", err)
		}
	}()
}

func retryWithBackoff(fn func() error, maxAttempts int, initialDelay time.Duration) error {
	var lastErr error
	delay := initialDelay

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
			if attempt < maxAttempts {
				log.Printf("Attempt %d failed, retrying in %v...", attempt, delay)
				time.Sleep(delay)
				delay *= 2
			}
		}
	}

	return lastErr
}

func respondWithError(w http.ResponseWriter, statusCode int, message string) {
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(APIResponse{
		Status:  "error",
		Message: message,
		Code:    statusCode,
	})
}

// WebSocket Hub Implementation

func newWebSocketHub() *WebSocketHub {
	return &WebSocketHub{
		clients:    make(map[*WebSocketClient]bool),
		broadcast:  make(chan interface{}, 256),
		register:   make(chan *WebSocketClient),
		unregister: make(chan *WebSocketClient),
	}
}

func (h *WebSocketHub) run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			log.Printf("✅ WebSocket client connected (total: %d)", len(h.clients))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			log.Printf("🔌 WebSocket client disconnected (total: %d)", len(h.clients))

		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					go func(c *WebSocketClient) {
						h.unregister <- c
					}(client)
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (h *WebSocketHub) close() {
	h.mu.Lock()
	for client := range h.clients {
		close(client.send)
	}
	h.mu.Unlock()
	close(h.broadcast)
}

func (c *WebSocketClient) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	}
}

func (c *WebSocketClient) writePump() {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.conn.WriteJSON(message); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ConfigCache methods

func (cc *ConfigCache) get() *AutoConfig {
	cc.mu.RLock()
	defer cc.mu.RUnlock()

	if time.Now().Before(cc.expiresAt) && cc.config != nil {
		return cc.config
	}

	return nil
}

func (cc *ConfigCache) set(config *AutoConfig, ttl time.Duration) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	cc.config = config
	cc.expiresAt = time.Now().Add(ttl)
}

func (cc *ConfigCache) invalidate() {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	cc.config = nil
	cc.expiresAt = time.Time{}
}