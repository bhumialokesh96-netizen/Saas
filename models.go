package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// ===== RATE LIMITING & BAN PREVENTION =====

// RateLimiter implements token bucket algorithm for rate limiting
type RateLimiter struct {
	mu          sync.RWMutex
	tokens      float64
	maxTokens   float64
	refillRate  float64
	lastRefill  time.Time
	windowStart time.Time
	count       int64
}

// ThrottleConfig controls message throttling
type ThrottleConfig struct {
	MinDelayBetweenMessages   time.Duration
	MaxDelayBetweenMessages   time.Duration
	MinDelayBetweenBatches    time.Duration
	MaxMessagePerHour         int
	MaxMessagePerDay          int
	MaxConsecutiveMessages    int
	MinRandomJitterPercent    int
	MaxRandomJitterPercent    int
}

// MessageQueue manages bulk messages with anti-ban strategies
type MessageQueue struct {
	mu          sync.RWMutex
	queue       []QueuedMessage
	processing  bool
	isPaused    bool
	pauseUntil  time.Time
	retryCount  map[string]int
	sentToday   map[string]int64
	sentThisHr  map[string]int64
	failureRate float64
}

// QueuedMessage represents a message in the queue
type QueuedMessage struct {
	ID            string
	PhoneNumber   string
	Message       string
	Status        string
	CreatedAt     time.Time
	ScheduledFor  time.Time
	SentAt        time.Time
	FailedAt      time.Time
	FailureReason string
	RetryCount    int
	MaxRetries    int
	Priority      int
}

// BanDetectionSystem monitors for ban signals
type BanDetectionSystem struct {
	mu                    sync.RWMutex
	consecutiveFailures   int32
	lastFailureTime       time.Time
	failurePattern        []time.Time
	suspiciousPatterns    int32
	warningLevel          int32
	isBanned              bool
	bannedAt              time.Time
	bannedReason          string
	checkInterval         time.Duration
	failureThreshold      int
	timeWindowForFailures time.Duration
}

// ConnectionHealthCheck monitors connection quality
type ConnectionHealthCheck struct {
	mu                      sync.RWMutex
	lastSuccessfulMessage   time.Time
	lastFailedMessage       time.Time
	totalMessagesSent       int64
	totalMessagesFailed     int64
	successRate             float64
	averageResponseTime     time.Duration
	consecutiveTimeouts     int32
	connectionQualityScore  float32
	lastHealthCheckTime     time.Time
}

// AdaptiveThrottler adjusts throttling based on responses
type AdaptiveThrottler struct {
	mu                   sync.RWMutex
	baseDelay            time.Duration
	currentDelay         time.Duration
	failureCount         int32
	successCount         int32
	lastAdjustmentTime   time.Time
	adjustmentInterval   time.Duration
	increaseRate         float64
	decreaseRate         float64
	minDelay             time.Duration
	maxDelay             time.Duration
	riskLevel            int32
}

// BulkMessagingConfig combines all anti-ban configurations
type BulkMessagingConfig struct {
	ThrottleConfig          *ThrottleConfig
	BanDetection            *BanDetectionSystem
	HealthCheck             *ConnectionHealthCheck
	AdaptiveThrottle        *AdaptiveThrottler
	EnableSmartTiming       bool
	EnableBehaviorMimicking bool
	EnableFailureRecovery   bool
	MaxRetriesPerMessage    int
	LogAllAttempts          bool
}

// MessageMetadata stores detailed message information
type MessageMetadata struct {
	MessageID           string
	PhoneNumber         string
	RecipientNumber     string
	Content             string
	SentAt              time.Time
	DeliveredAt         time.Time
	ReadAt              time.Time
	FailureReason       string
	DeviceUsed          string
	ProxyUsed           string
	ResponseTime        time.Duration
	WasThrottled        bool
	ThrottleDelay       time.Duration
	RetryAttempt        int
	BanRiskScore        float32
	ConnectionQuality   float32
}

// ===== HELPER FUNCTIONS =====

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(maxTokens float64, refillRate float64) *RateLimiter {
	return &RateLimiter{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

// Allow checks if operation is allowed by rate limiter
func (rl *RateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.lastRefill).Seconds()
	rl.tokens = min(rl.maxTokens, rl.tokens+elapsed*rl.refillRate)
	rl.lastRefill = now

	if rl.tokens >= 1 {
		rl.tokens--
		return true
	}
	return false
}

// WaitForToken blocks until a token is available
func (rl *RateLimiter) WaitForToken() {
	for !rl.Allow() {
		time.Sleep(100 * time.Millisecond)
	}
}

// NewMessageQueue creates a new message queue
func NewMessageQueue() *MessageQueue {
	return &MessageQueue{
		queue:       []QueuedMessage{},
		retryCount:  make(map[string]int),
		sentToday:   make(map[string]int64),
		sentThisHr:  make(map[string]int64),
	}
}

// AddMessage adds a message to the queue
func (mq *MessageQueue) AddMessage(msg QueuedMessage) {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	msg.Status = "pending"
	msg.CreatedAt = time.Now()
	mq.queue = append(mq.queue, msg)
}

// GetPendingMessages returns all pending messages
func (mq *MessageQueue) GetPendingMessages() []QueuedMessage {
	mq.mu.RLock()
	defer mq.mu.RUnlock()

	var pending []QueuedMessage
	for _, msg := range mq.queue {
		if msg.Status == "pending" || msg.Status == "retrying" {
			pending = append(pending, msg)
		}
	}
	return pending
}

// UpdateMessageStatus updates the status of a message
func (mq *MessageQueue) UpdateMessageStatus(messageID, status string) {
	mq.mu.Lock()
	defer mq.mu.Unlock()

	for i, msg := range mq.queue {
		if msg.ID == messageID {
			mq.queue[i].Status = status
			if status == "sent" {
				mq.queue[i].SentAt = time.Now()
			}
			break
		}
	}
}

// NewBanDetectionSystem creates a new ban detection system
func NewBanDetectionSystem() *BanDetectionSystem {
	return &BanDetectionSystem{
		failurePattern:        []time.Time{},
		checkInterval:         5 * time.Second,
		failureThreshold:      5,
		timeWindowForFailures: 1 * time.Minute,
	}
}

// RecordFailure records a failed message attempt
func (bds *BanDetectionSystem) RecordFailure(reason string) {
	bds.mu.Lock()
	defer bds.mu.Unlock()

	atomic.AddInt32(&bds.consecutiveFailures, 1)
	bds.lastFailureTime = time.Now()
	bds.failurePattern = append(bds.failurePattern, time.Now())

	cutoff := time.Now().Add(-bds.timeWindowForFailures)
	var recent []time.Time
	for _, t := range bds.failurePattern {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	bds.failurePattern = recent

	if len(recent) >= bds.failureThreshold {
		atomic.StoreInt32(&bds.warningLevel, 50)
	}

	if reason == "banned" || reason == "account_suspended" {
		bds.isBanned = true
		bds.bannedAt = time.Now()
		bds.bannedReason = reason
		atomic.StoreInt32(&bds.warningLevel, 100)
	}
}

// RecordSuccess records a successful message
func (bds *BanDetectionSystem) RecordSuccess() {
	bds.mu.Lock()
	defer bds.mu.Unlock()

	atomic.AddInt32(&bds.consecutiveFailures, -1)
	if atomic.LoadInt32(&bds.consecutiveFailures) < 0 {
		atomic.StoreInt32(&bds.consecutiveFailures, 0)
	}

	current := atomic.LoadInt32(&bds.warningLevel)
	if current > 10 {
		atomic.AddInt32(&bds.warningLevel, -5)
	}
}

// GetBanRisk returns current ban risk level
func (bds *BanDetectionSystem) GetBanRisk() string {
	level := atomic.LoadInt32(&bds.warningLevel)
	if level >= 80 {
		return "critical"
	} else if level >= 50 {
		return "high"
	} else if level >= 30 {
		return "medium"
	}
	return "low"
}

// IsBanned checks if account is banned
func (bds *BanDetectionSystem) IsBanned() bool {
	bds.mu.RLock()
	defer bds.mu.RUnlock()
	return bds.isBanned
}

// NewConnectionHealthCheck creates a new health check
func NewConnectionHealthCheck() *ConnectionHealthCheck {
	return &ConnectionHealthCheck{
		lastSuccessfulMessage:  time.Now(),
		connectionQualityScore: 100,
	}
}

// RecordMessageSent records a successful message send
func (chc *ConnectionHealthCheck) RecordMessageSent() {
	chc.mu.Lock()
	defer chc.mu.Unlock()

	atomic.AddInt64(&chc.totalMessagesSent, 1)
	chc.lastSuccessfulMessage = time.Now()
	atomic.StoreInt32(&chc.consecutiveTimeouts, 0)

	total := atomic.LoadInt64(&chc.totalMessagesSent) + atomic.LoadInt64(&chc.totalMessagesFailed)
	if total > 0 {
		chc.successRate = float64(atomic.LoadInt64(&chc.totalMessagesSent)) / float64(total) * 100
		chc.connectionQualityScore = float32(chc.successRate)
	}
}

// RecordMessageFailed records a failed message send
func (chc *ConnectionHealthCheck) RecordMessageFailed(responseTime time.Duration) {
	chc.mu.Lock()
	defer chc.mu.Unlock()

	atomic.AddInt64(&chc.totalMessagesFailed, 1)
	chc.lastFailedMessage = time.Now()

	if responseTime > 30*time.Second {
		atomic.AddInt32(&chc.consecutiveTimeouts, 1)
	}

	total := atomic.LoadInt64(&chc.totalMessagesSent) + atomic.LoadInt64(&chc.totalMessagesFailed)
	if total > 0 {
		chc.successRate = float64(atomic.LoadInt64(&chc.totalMessagesSent)) / float64(total) * 100
		chc.connectionQualityScore = float32(chc.successRate)
	}
}

// GetHealthScore returns current health score (0-100)
func (chc *ConnectionHealthCheck) GetHealthScore() float32 {
	chc.mu.RLock()
	defer chc.mu.RUnlock()
	return chc.connectionQualityScore
}

// NewAdaptiveThrottler creates a new adaptive throttler
func NewAdaptiveThrottler(baseDelay time.Duration) *AdaptiveThrottler {
	return &AdaptiveThrottler{
		baseDelay:          baseDelay,
		currentDelay:       baseDelay,
		lastAdjustmentTime: time.Now(),
		adjustmentInterval: 30 * time.Second,
		increaseRate:       1.5,
		decreaseRate:       0.8,
		minDelay:           100 * time.Millisecond,
		maxDelay:           60 * time.Second,
	}
}

// GetCurrentDelay returns the current throttle delay
func (at *AdaptiveThrottler) GetCurrentDelay() time.Duration {
	at.mu.RLock()
	defer at.mu.RUnlock()
	return at.currentDelay
}

// AdjustThrottling adjusts throttling based on success/failure
func (at *AdaptiveThrottler) AdjustThrottling(success bool) {
	at.mu.Lock()
	defer at.mu.Unlock()

	now := time.Now()
	if now.Sub(at.lastAdjustmentTime) < at.adjustmentInterval {
		return
	}

	if success {
		atomic.AddInt32(&at.successCount, 1)
		if atomic.LoadInt32(&at.successCount) > 5 {
			at.currentDelay = time.Duration(float64(at.currentDelay) * at.decreaseRate)
			if at.currentDelay < at.minDelay {
				at.currentDelay = at.minDelay
			}
			atomic.StoreInt32(&at.successCount, 0)
		}
	} else {
		atomic.AddInt32(&at.failureCount, 1)
		if atomic.LoadInt32(&at.failureCount) > 2 {
			at.currentDelay = time.Duration(float64(at.currentDelay) * at.increaseRate)
			if at.currentDelay > at.maxDelay {
				at.currentDelay = at.maxDelay
			}
			atomic.AddInt32(&at.riskLevel, 10)
			atomic.StoreInt32(&at.failureCount, 0)
		}
	}

	at.lastAdjustmentTime = now
}

// Helper function
func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
