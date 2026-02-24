package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// BulkMessage represents a bulk message request
type BulkMessage struct {
	PhoneNumbers []string `json:"phone_numbers"`
	Message      string   `json:"message"`
	Delay        int      `json:"delay"`
	Priority     int      `json:"priority"`
}

// BulkMessagingRequest represents a bulk messaging configuration request
type BulkMessagingRequest struct {
	Strategy string            `json:"strategy"`
	Messages []BulkMessage     `json:"messages"`
	Options  map[string]interface{} `json:"options"`
}

var bulkEngine *BulkMessagingEngine

// init initializes the bulk messaging engine
func initBulkMessagingEngine() {
	bulkEngine = NewBulkMessagingEngine()
	bulkEngine.Start()
}

// handleBulkSend handles bulk message sending
func handleBulkSend(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req BulkMessagingRequest
	body, err := io.ReadAll(r.Body)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid body")
		return
	}
	defer r.Body.Close()

	if err := json.Unmarshal(body, &req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if req.Strategy == "" {
		req.Strategy = "moderate"
	}

	if len(req.Messages) == 0 {
		respondWithError(w, http.StatusBadRequest, "No messages provided")
		return
	}

	if len(req.Messages) > 1000 {
		respondWithError(w, http.StatusBadRequest, "Too many messages (max 1000 per request)")
		return
	}

	strategy := &MessageStrategy{
		Type: req.Strategy,
	}
	bulkEngine.SetStrategy(strategy)

	addedCount := 0
	for _, bulkMsg := range req.Messages {
		for _, phoneNumber := range bulkMsg.PhoneNumbers {
			msg := QueuedMessage{
				ID:          fmt.Sprintf("%d-%s", len(bulkEngine.queue.queue), phoneNumber),
				PhoneNumber: strings.TrimSpace(phoneNumber),
				Message:     bulkMsg.Message,
				Priority:    bulkMsg.Priority,
			}

			if msg.PhoneNumber != "" && len(msg.Message) > 0 {
				bulkEngine.queue.AddMessage(msg)
				addedCount++
			}
		}
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "success",
		"messages_queued": addedCount,
		"strategy":        req.Strategy,
	})
}

// handleBulkStatus returns bulk messaging status
func handleBulkStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	stats := bulkEngine.GetStats()
	stats["status"] = "success"

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(stats)
}

// handleBulkPause pauses bulk messaging
func handleBulkPause(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	bulkEngine.queue.mu.Lock()
	bulkEngine.queue.isPaused = true
	bulkEngine.queue.pauseUntil = time.Now().Add(30 * time.Minute)
	bulkEngine.queue.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Bulk messaging paused",
	})
}

// handleBulkResume resumes bulk messaging
func handleBulkResume(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	bulkEngine.queue.mu.Lock()
	bulkEngine.queue.isPaused = false
	bulkEngine.queue.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Bulk messaging resumed",
	})
}

// handleBulkCancel cancels all pending messages
func handleBulkCancel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	bulkEngine.queue.mu.Lock()
	bulkEngine.queue.queue = []QueuedMessage{}
	bulkEngine.queue.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "All pending messages cancelled",
	})
}

// handleBanStatus returns ban detection status
func handleBanStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	isBanned := bulkEngine.banDetection.IsBanned()
	riskLevel := bulkEngine.banDetection.GetBanRisk()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":                "success",
		"is_banned":             isBanned,
		"risk_level":            riskLevel,
		"health":                bulkEngine.healthCheck.GetHealthScore(),
		"consecutive_failures":  bulkEngine.banDetection.consecutiveFailures,
	})
}

// setupBulkMessagingRoutes registers all bulk messaging routes
func setupBulkMessagingRoutes() {
	http.HandleFunc("/api/bulk/send", handleBulkSend)
	http.HandleFunc("/api/bulk/status", handleBulkStatus)
	http.HandleFunc("/api/bulk/pause", handleBulkPause)
	http.HandleFunc("/api/bulk/resume", handleBulkResume)
	http.HandleFunc("/api/bulk/cancel", handleBulkCancel)
	http.HandleFunc("/api/bulk/ban-status", handleBanStatus)
	log.Println("✅ Bulk messaging routes registered")
}