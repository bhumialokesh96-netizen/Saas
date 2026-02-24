package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"time"
)

// BulkMessagingEngine handles intelligent bulk messaging with anti-ban strategies
type BulkMessagingEngine struct {
	config              *BulkMessagingConfig
	queue               *MessageQueue
	banDetection        *BanDetectionSystem
	healthCheck         *ConnectionHealthCheck
	adaptiveThrottle    *AdaptiveThrottler
	rateLimiter         *RateLimiter
	currentStrategy     *MessageStrategy
	isRunning           bool
	ctx                 context.Context
	cancel              context.CancelFunc
	messagesSentToday   int64
	messagesSentThisHr  int64
	lastHourReset       time.Time
	lastDayReset        time.Time
}

// MessageStrategy defines how to send messages
type MessageStrategy struct {
	Type                string
	DelayBetweenMessage time.Duration
	BatchSize           int
	PauseAfterBatch     time.Duration
	MaxMessagesPerHour  int
	MaxMessagesPerDay   int
	UseProxyRotation    bool
	UseDeviceRotation   bool
	RandomizeDelay      bool
	CheckConnection     bool
	RetryStrategy       string
}

// NewBulkMessagingEngine creates a new bulk messaging engine
func NewBulkMessagingEngine() *BulkMessagingEngine {
	ctx, cancel := context.WithCancel(context.Background())

	config := &BulkMessagingConfig{
		ThrottleConfig: &ThrottleConfig{
			MinDelayBetweenMessages:   2 * time.Second,
			MaxDelayBetweenMessages:   8 * time.Second,
			MinDelayBetweenBatches:    15 * time.Second,
			MaxMessagePerHour:         30,
			MaxMessagePerDay:          200,
			MaxConsecutiveMessages:    5,
			MinRandomJitterPercent:    10,
			MaxRandomJitterPercent:    40,
		},
		BanDetection:            NewBanDetectionSystem(),
		HealthCheck:             NewConnectionHealthCheck(),
		AdaptiveThrottle:        NewAdaptiveThrottler(5 * time.Second),
		EnableSmartTiming:       true,
		EnableBehaviorMimicking: true,
		EnableFailureRecovery:   true,
		MaxRetriesPerMessage:    3,
		LogAllAttempts:          true,
	}

	engine := &BulkMessagingEngine{
		config:             config,
		queue:              NewMessageQueue(),
		banDetection:       config.BanDetection,
		healthCheck:        config.HealthCheck,
		adaptiveThrottle:   config.AdaptiveThrottle,
		rateLimiter:        NewRateLimiter(1, 0.2),
		isRunning:          false,
		ctx:                ctx,
		cancel:             cancel,
		lastHourReset:      time.Now(),
		lastDayReset:       time.Now(),
	}

	go engine.monitorHealth()

	return engine
}

// Start begins the bulk messaging engine
func (bme *BulkMessagingEngine) Start() {
	if bme.isRunning {
		return
	}

	bme.isRunning = true
	go bme.processingLoop()
	log.Println("✅ Bulk messaging engine started")
}

// Stop stops the bulk messaging engine
func (bme *BulkMessagingEngine) Stop() {
	if !bme.isRunning {
		return
	}

	bme.isRunning = false
	bme.cancel()
	log.Println("⏹️  Bulk messaging engine stopped")
}

// processingLoop is the main message processing loop
func (bme *BulkMessagingEngine) processingLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-bme.ctx.Done():
			return
		case <-ticker.C:
			if bme.isRunning {
				bme.processBatch()
			}
		}
	}
}

// processBatch processes a batch of messages
func (bme *BulkMessagingEngine) processBatch() {
	if bme.banDetection.IsBanned() {
		log.Println("⛔ Account is banned. Stopping bulk messaging.")
		bme.Stop()
		return
	}

	if !bme.shouldProcessMessages() {
		return
	}

	pending := bme.queue.GetPendingMessages()
	if len(pending) == 0 {
		return
	}

	batchSize := bme.calculateOptimalBatchSize()

	for i := 0; i < minInt(int(batchSize), len(pending)); i++ {
		msg := pending[i]

		delay := bme.calculateMessageDelay(i, batchSize)
		time.Sleep(delay)

		success := bme.sendMessageWithRetry(&msg)

		if success {
			bme.queue.UpdateMessageStatus(msg.ID, "sent")
			bme.healthCheck.RecordMessageSent()
			bme.banDetection.RecordSuccess()
			bme.adaptiveThrottle.AdjustThrottling(true)
			bme.messagesSentToday++
			bme.messagesSentThisHr++

			if bme.config.LogAllAttempts {
				log.Printf("✅ Message sent to %s (Attempt: %d, Health: %.1f%%)",
					msg.PhoneNumber, msg.RetryCount+1, bme.healthCheck.GetHealthScore())
			}
		} else {
			if msg.RetryCount < bme.config.MaxRetriesPerMessage {
				msg.RetryCount++
				bme.queue.UpdateMessageStatus(msg.ID, "retrying")

				if bme.config.LogAllAttempts {
					log.Printf("🔄 Message queued for retry (Phone: %s, Retry: %d/%d)",
						msg.PhoneNumber, msg.RetryCount, bme.config.MaxRetriesPerMessage)
				}
			} else {
				bme.queue.UpdateMessageStatus(msg.ID, "failed")
				if bme.config.LogAllAttempts {
					log.Printf("❌ Message failed after %d retries (Phone: %s)",
						msg.RetryCount, msg.PhoneNumber)
				}
			}

			bme.healthCheck.RecordMessageFailed(5 * time.Second)
			bme.banDetection.RecordFailure("send_failed")
			bme.adaptiveThrottle.AdjustThrottling(false)
		}

		if (i+1)%bme.config.ThrottleConfig.MaxConsecutiveMessages == 0 {
			pauseDuration := bme.calculatePauseDuration()
			log.Printf("⏸️  Pausing for %v after %d messages", pauseDuration, bme.config.ThrottleConfig.MaxConsecutiveMessages)
			time.Sleep(pauseDuration)
		}
	}
}

// sendMessageWithRetry sends a message with retry logic
func (bme *BulkMessagingEngine) sendMessageWithRetry(msg *QueuedMessage) bool {
	for attempt := 0; attempt <= bme.config.MaxRetriesPerMessage; attempt++ {
		if attempt > 0 {
			backoffDuration := time.Duration(math.Pow(2, float64(attempt))) * time.Second
			log.Printf("⏳ Retrying in %v... (Attempt %d)", backoffDuration, attempt+1)
			time.Sleep(backoffDuration)
		}

		if bme.sendMessage(msg) {
			return true
		}
	}

	return false
}

// sendMessage sends a single message
func (bme *BulkMessagingEngine) sendMessage(msg *QueuedMessage) bool {
	if bme.healthCheck.GetHealthScore() < 20 {
		log.Println("⚠️  Connection health too low. Pausing...")
		time.Sleep(30 * time.Second)
		return false
	}

	if bme.messagesSentThisHr >= int64(bme.config.ThrottleConfig.MaxMessagePerHour) {
		log.Println("⚠️  Hourly limit reached. Waiting...")
		time.Sleep(30 * time.Second)
		return false
	}

	if bme.config.EnableBehaviorMimicking {
		bme.mimicHumanBehavior()
	}

	jid := formatPhoneNumber(msg.PhoneNumber)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if bme.config.LogAllAttempts {
		log.Printf("📤 Sending message to %s (Risk: %s)", msg.PhoneNumber, bme.banDetection.GetBanRisk())
	}

	msgProto := &waProto.Message{
		Conversation: &msg.Message,
	}

	startTime := time.Now()
	_, err := client.SendMessage(ctx, jid, msgProto)
	_ = time.Since(startTime)

	if err != nil {
		return false
	}

	return true
}

// shouldProcessMessages checks if we should continue processing
func (bme *BulkMessagingEngine) shouldProcessMessages() bool {
	if bme.banDetection.IsBanned() {
		return false
	}

	if bme.queue.isPaused && time.Now().Before(bme.queue.pauseUntil) {
		return false
	}

	if time.Now().Sub(bme.lastHourReset) > 1*time.Hour {
		bme.messagesSentThisHr = 0
		bme.lastHourReset = time.Now()
	}

	if bme.messagesSentThisHr >= int64(bme.config.ThrottleConfig.MaxMessagePerHour) {
		log.Println("⚠️  Hourly limit reached")
		return false
	}

	if time.Now().Sub(bme.lastDayReset) > 24*time.Hour {
		bme.messagesSentToday = 0
		bme.lastDayReset = time.Now()
	}

	if bme.messagesSentToday >= int64(bme.config.ThrottleConfig.MaxMessagePerDay) {
		log.Println("⚠️  Daily limit reached")
		return false
	}

	if !client.IsConnected() {
		return false
	}

	return true
}

// calculateOptimalBatchSize calculates how many messages to send
func (bme *BulkMessagingEngine) calculateOptimalBatchSize() int64 {
	health := bme.healthCheck.GetHealthScore()

	baseSize := int64(5)

	if health < 50 {
		baseSize = 2
	} else if health < 75 {
		baseSize = 3
	}

	return baseSize
}

// calculateMessageDelay calculates delay between messages
func (bme *BulkMessagingEngine) calculateMessageDelay(messageIndex int, batchSize int64) time.Duration {
	baseDelay := bme.config.AdaptiveThrottle.GetCurrentDelay()

	if bme.config.EnableSmartTiming {
		if messageIndex == 0 {
			baseDelay = baseDelay * 2
		}
	}

	jitterPercent := rand.Intn(bme.config.ThrottleConfig.MaxRandomJitterPercent-bme.config.ThrottleConfig.MinRandomJitterPercent) +
		bme.config.ThrottleConfig.MinRandomJitterPercent
	jitter := time.Duration(float64(baseDelay) * float64(jitterPercent) / 100)

	finalDelay := baseDelay + jitter

	if finalDelay < bme.config.ThrottleConfig.MinDelayBetweenMessages {
		finalDelay = bme.config.ThrottleConfig.MinDelayBetweenMessages
	}
	if finalDelay > bme.config.ThrottleConfig.MaxDelayBetweenMessages {
		finalDelay = bme.config.ThrottleConfig.MaxDelayBetweenMessages
	}

	return finalDelay
}

// calculatePauseDuration calculates pause time between batches
func (bme *BulkMessagingEngine) calculatePauseDuration() time.Duration {
	basePause := bme.config.ThrottleConfig.MinDelayBetweenBatches

	if bme.healthCheck.GetHealthScore() < 60 {
		basePause = basePause * 2
	}

	randomFactor := 0.8 + (rand.Float64() * 0.4)
	return time.Duration(float64(basePause) * randomFactor)
}

// mimicHumanBehavior adds delays that mimic human typing/sending patterns
func (bme *BulkMessagingEngine) mimicHumanBehavior() {
	if rand.Float64() < 0.3 {
		typing := time.Duration(rand.Intn(3000-500)+500) * time.Millisecond
		time.Sleep(typing)
	}

	if rand.Float64() < 0.1 {
		thinking := time.Duration(rand.Intn(5000-1000)+1000) * time.Millisecond
		time.Sleep(thinking)
	}
}

// monitorHealth continuously monitors connection and ban status
func (bme *BulkMessagingEngine) monitorHealth() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-bme.ctx.Done():
			return
		case <-ticker.C:
			bme.checkSystemHealth()
		}
	}
}

// checkSystemHealth checks overall system health
func (bme *BulkMessagingEngine) checkSystemHealth() {
	health := bme.healthCheck.GetHealthScore()
	riskLevel := bme.banDetection.GetBanRisk()

	if bme.config.LogAllAttempts {
		log.Printf("📊 Health: %.1f%% | Risk: %s | Sent: %d/hr, %d/day",
			health, riskLevel, bme.messagesSentThisHr, bme.messagesSentToday)
	}

	if health < 20 {
		log.Println("⛔ Critical health drop. Pausing...")
		bme.queue.isPaused = true
		bme.queue.pauseUntil = time.Now().Add(5 * time.Minute)
	}

	if riskLevel == "critical" {
		log.Println("⛔ Critical ban risk detected. Stopping...")
		bme.Stop()
	}
}

// SetStrategy sets the messaging strategy
func (bme *BulkMessagingEngine) SetStrategy(strategy *MessageStrategy) {
	bme.currentStrategy = strategy

	switch strategy.Type {
	case "aggressive":
		bme.config.ThrottleConfig.MinDelayBetweenMessages = 1 * time.Second
		bme.config.ThrottleConfig.MaxMessagePerHour = 60
		bme.config.ThrottleConfig.MaxMessagePerDay = 500
	case "moderate":
		bme.config.ThrottleConfig.MinDelayBetweenMessages = 3 * time.Second
		bme.config.ThrottleConfig.MaxMessagePerHour = 30
		bme.config.ThrottleConfig.MaxMessagePerDay = 200
	case "conservative":
		bme.config.ThrottleConfig.MinDelayBetweenMessages = 5 * time.Second
		bme.config.ThrottleConfig.MaxMessagePerHour = 15
		bme.config.ThrottleConfig.MaxMessagePerDay = 100
	}

	log.Printf("📋 Strategy changed to: %s", strategy.Type)
}

// GetStats returns current statistics
func (bme *BulkMessagingEngine) GetStats() map[string]interface{} {
	return map[string]interface{}{
		"messages_sent_today":     bme.messagesSentToday,
		"messages_sent_this_hour": bme.messagesSentThisHr,
		"health_score":            bme.healthCheck.GetHealthScore(),
		"ban_risk":                bme.banDetection.GetBanRisk(),
		"is_banned":               bme.banDetection.IsBanned(),
		"pending_messages":        len(bme.queue.GetPendingMessages()),
		"adaptive_delay":          bme.adaptiveThrottle.GetCurrentDelay().String(),
	}
}
