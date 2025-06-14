package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/husmancristian/TA_GEAMAN/pkg/models" // Adjust import path
	"github.com/husmancristian/TA_GEAMAN/pkg/queue"  // Import queue package for AckNacker

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go" // RabbitMQ client
)

const (
	// Exchange name (can be customized)
	jobsExchange = "test_jobs_exchange"
	// Type of exchange (direct allows routing based on key)
	exchangeType = "direct"
	// Max priority level for queues (adjust as needed)
	maxPriority = 10
	// Content type for messages
	contentTypeJSON = "application/json"
	// Consumer tag prefix
	consumerTagPrefix = "test-runner-consumer-"
)

// Ensure RabbitMQManager implements queue.Manager interface at compile time
var _ queue.Manager = (*RabbitMQManager)(nil)

// RabbitMQManager implements the queue.Manager interface using RabbitMQ.
type RabbitMQManager struct {
	conn *amqp.Connection
	// channel *amqp.Channel // REMOVED: Avoid storing a single shared channel for all ops
	logger *slog.Logger
	// Map to keep track of declared queues for projects to avoid redeclaring constantly
	declaredQueues sync.Map // map[string]bool
	// Mutex for operations that might still need shared state if any (e.g., declaredQueues)
	mu sync.Mutex
}

// deliveryAckNacker implements the queue.AckNacker interface for RabbitMQ deliveries.
type deliveryAckNacker struct {
	deliveryTag uint64
	channel     *amqp.Channel // Reference to the channel used for THIS delivery
	logger      *slog.Logger
	closed      bool // Track if ack/nack was already called
	mu          sync.Mutex
}

// Ack acknowledges the message. Idempotent.
func (a *deliveryAckNacker) Ack() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		a.logger.Warn("Attempted to Ack already closed AckNacker", slog.Uint64("deliveryTag", a.deliveryTag))
		return nil // Or return an error? For now, just log and ignore.
	}
	err := a.channel.Ack(a.deliveryTag, false) // multiple = false
	if err != nil {
		a.logger.Error("Failed to ACK message", slog.Uint64("deliveryTag", a.deliveryTag), slog.String("error", err.Error()))
	} else {
		a.closed = true // Mark as closed after successful ack
	}
	// We might consider closing the channel associated with this AckNacker here if it was temporary
	// For GetNextJob using channel.Get, the channel might be longer lived if GetNextJob reuses it.
	// Let's assume the channel used by GetNextJob is managed separately for now.
	return err
}

// Nack negatively acknowledges the message. Idempotent.
func (a *deliveryAckNacker) Nack(requeue bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		a.logger.Warn("Attempted to Nack already closed AckNacker", slog.Uint64("deliveryTag", a.deliveryTag))
		return nil
	}
	err := a.channel.Nack(a.deliveryTag, false, requeue) // multiple = false
	if err != nil {
		a.logger.Error("Failed to NACK message", slog.Uint64("deliveryTag", a.deliveryTag), slog.Bool("requeue", requeue), slog.String("error", err.Error()))
	} else {
		a.closed = true // Mark as closed after successful nack
	}
	// Similar consideration for closing the channel as in Ack()
	return err
}

// NewRabbitMQManager creates a new RabbitMQ queue manager.
// It establishes a connection. Channels are created per operation or as needed.
func NewRabbitMQManager(url string, logger *slog.Logger) (*RabbitMQManager, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}
	// Log connection success
	logger.Info("RabbitMQ connection established")

	// Setup close handler to log unexpected connection closures
	closeChan := make(chan *amqp.Error)
	conn.NotifyClose(closeChan)
	go func() {
		amqpErr := <-closeChan
		if amqpErr != nil {
			logger.Error("RabbitMQ connection closed unexpectedly", slog.String("error", amqpErr.Error()))
			// TODO: Implement reconnection logic here if desired
		} else {
			logger.Info("RabbitMQ connection closed normally")
		}
	}()

	manager := &RabbitMQManager{
		conn: conn,
		// channel: nil, // Don't store a shared channel
		logger: logger,
	}

	// Declare the exchange using a temporary channel during initialization
	err = manager.declareExchange()
	if err != nil {
		conn.Close()
		return nil, err // Return error from declareExchange
	}

	return manager, nil
}

// declareExchange ensures the main exchange exists. Uses a temporary channel.
func (m *RabbitMQManager) declareExchange() error {
	ch, err := m.conn.Channel()
	if err != nil {
		return fmt.Errorf("failed to open temporary channel for exchange declare: %w", err)
	}
	defer ch.Close()

	err = ch.ExchangeDeclare(
		jobsExchange, // name
		exchangeType, // type
		true,         // durable
		false,        // auto-deleted
		false,        // internal
		false,        // no-wait
		nil,          // arguments
	)
	if err != nil {
		return fmt.Errorf("failed to declare exchange '%s': %w", jobsExchange, err)
	}
	m.logger.Info("Declared exchange", slog.String("exchange", jobsExchange))
	return nil
}

// Close closes the RabbitMQ connection.
func (m *RabbitMQManager) Close() error {
	m.logger.Info("Closing RabbitMQ connection")
	// Channel closing is handled per-operation or implicitly when connection closes
	if m.conn != nil {
		if err := m.conn.Close(); err != nil {
			m.logger.Error("Failed to close RabbitMQ connection", slog.String("error", err.Error()))
			return err // Return connection close error
		}
	}
	return nil
}

// declareProjectQueue ensures a queue for the project exists and is bound to the exchange.
// Uses a temporary channel.
func (m *RabbitMQManager) declareProjectQueue(project string) error {
	// Check cache first (thread-safe)
	if _, loaded := m.declaredQueues.Load(project); loaded {
		return nil // Already declared in this session
	}

	// Use mutex to ensure only one goroutine declares/binds a specific queue at a time
	// even though channel usage below is temporary. Protects the sync.Map write.
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double check cache after acquiring lock
	if _, loaded := m.declaredQueues.Load(project); loaded {
		return nil
	}

	// Get a temporary channel for this operation
	ch, err := m.conn.Channel()
	if err != nil {
		return fmt.Errorf("failed to open temporary channel for queue declare: %w", err)
	}
	defer ch.Close() // Ensure temporary channel is closed

	queueName := project // Use project name as queue name directly

	// Declare the queue with priority support
	args := amqp.Table{"x-max-priority": int32(maxPriority)} // Needs to be int32 for AMQP table
	_, err = ch.QueueDeclare(
		queueName, // name
		true,      // durable (queue survives server restart)
		false,     // delete when unused
		false,     // exclusive (only this connection can use it)
		false,     // no-wait
		args,      // arguments (for priority)
	)
	if err != nil {
		// Don't store in map if declaration failed
		return fmt.Errorf("failed to declare queue '%s': %w", queueName, err)
	}

	// Bind the queue to the exchange using the project name as the routing key
	err = ch.QueueBind(
		queueName,    // queue name
		queueName,    // routing key (project name)
		jobsExchange, // exchange
		false,
		nil,
	)
	if err != nil {
		// Don't store in map if binding failed
		return fmt.Errorf("failed to bind queue '%s' to exchange '%s': %w", queueName, jobsExchange, err)
	}

	// Store in map only after successful declare and bind
	m.declaredQueues.Store(queueName, true)

	m.logger.Info("Declared and bound queue", slog.String("queue", queueName), slog.String("exchange", jobsExchange))
	return nil
}

// EnqueueJob publishes a new test job message using a temporary channel.
func (m *RabbitMQManager) EnqueueJob(project string, details map[string]interface{}, userPriority uint8) (string, error) {
	// Ensure the queue exists for this project (uses its own temp channel)
	if err := m.declareProjectQueue(project); err != nil {
		return "", err
	}

	// Get a temporary channel for publishing
	ch, err := m.conn.Channel()
	if err != nil {
		return "", fmt.Errorf("failed to open temporary channel for publish: %w", err)
	}
	defer ch.Close() // Ensure temporary channel is closed

	// Clamp userPriority to be within 0 and maxPriority
	// Lower userPriority value means higher importance.
	effectiveUserPriority := userPriority
	if effectiveUserPriority > uint8(maxPriority) { // maxPriority is int, cast for comparison
		m.logger.Warn("User priority exceeds max configured queue priority, clamping to lowest effective user priority.",
			slog.Uint64("user_priority", uint64(userPriority)),
			slog.Int("max_queue_priority_levels", maxPriority),
			slog.Uint64("clamped_user_priority", uint64(maxPriority)))
		effectiveUserPriority = uint8(maxPriority)
	}

	// Invert priority for RabbitMQ: lower user number = higher RabbitMQ number
	// e.g., user_priority 0 (highest) -> rabbitmq_priority maxPriority (e.g., 10)
	// e.g., user_priority maxPriority (lowest) -> rabbitmq_priority 0
	rabbitmqMessagePriority := uint8(maxPriority) - effectiveUserPriority

	jobID := uuid.NewString() // Generate unique ID
	jobMsg := models.JobMessage{
		ID:         jobID,
		Project:    project,
		Details:    details,
		Priority:   userPriority, // Store the original user-facing priority in the message
		EnqueuedAt: time.Now().UTC(),
	}

	body, err := json.Marshal(jobMsg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal job message to JSON: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // Context for publish timeout
	defer cancel()

	err = ch.PublishWithContext(ctx,
		jobsExchange, // exchange
		project,      // routing key (project name)
		false,        // mandatory
		false,        // immediate
		amqp.Publishing{
			ContentType:  contentTypeJSON,
			DeliveryMode: amqp.Persistent,         // Make message persistent
			Priority:     rabbitmqMessagePriority, // Set RabbitMQ message priority
			Timestamp:    time.Now().UTC(),
			Body:         body,
			MessageId:    jobID, // Use job ID as message ID
		})
	if err != nil {
		return "", fmt.Errorf("failed to publish message for project '%s': %w", project, err)
	}

	m.logger.Info("Enqueued job",
		slog.String("job_id", jobID),
		slog.String("project", project),
		slog.Uint64("user_priority", uint64(userPriority)),
		slog.Uint64("rabbitmq_priority", uint64(rabbitmqMessagePriority)),
	)
	return jobID, nil
}

// GetNextJob consumes the next available message from the project queue.
// It uses a dedicated channel for the Get operation and returns an AckNacker
// which holds a reference to that *same* channel for Ack/Nack operations.
// The caller is responsible for ensuring Ack/Nack is called.
func (m *RabbitMQManager) GetNextJob(project string) (*models.TestJob, queue.AckNacker, error) {
	// Ensure the queue exists (uses its own temp channel)
	if err := m.declareProjectQueue(project); err != nil {
		m.logger.Warn("Attempted to get job from non-existent or undeclared queue", slog.String("project", project), slog.String("error", err.Error()))
		return nil, nil, nil // Treat as no job available
	}

	// Get a channel specifically for this Get operation and subsequent Ack/Nack
	// This channel should ideally live as long as the AckNacker.
	// For simplicity here, we create it, but managing its lifecycle perfectly
	// without leaking can be tricky. Let's assume it's closed implicitly
	// when the connection closes if Ack/Nack isn't called (not ideal).
	// A better approach might involve a pool or more explicit channel management.
	ch, err := m.conn.Channel()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open channel for GetNextJob: %w", err)
	}
	// We don't defer ch.Close() here because the AckNacker needs it.

	queueName := project
	// Use Qos to ensure runner only prefetches one message at a time on this channel
	err = ch.Qos(1, 0, false)
	if err != nil {
		ch.Close() // Close channel if Qos fails
		return nil, nil, fmt.Errorf("failed to set QoS: %w", err)
	}

	// Use channel.Get for a single pull
	msg, ok, err := ch.Get(queueName, false) // autoAck = false
	if err != nil {
		ch.Close() // Close channel on error
		return nil, nil, fmt.Errorf("failed to get message from queue '%s': %w", queueName, err)
	}

	// Check if a message was actually retrieved
	if !ok {
		ch.Close() // Close channel if no message retrieved
		return nil, nil, nil
	}

	// Create the AckNacker for this delivery, passing the channel it needs
	ackNacker := &deliveryAckNacker{
		deliveryTag: msg.DeliveryTag,
		channel:     ch, // Pass the channel used for Get
		logger:      m.logger.With(slog.String("job_id", msg.MessageId)),
	}

	// Message received, parse it
	var jobMsg models.JobMessage
	err = json.Unmarshal(msg.Body, &jobMsg)
	if err != nil {
		// Failed to parse, potentially bad message. Nack it? Dead-letter?
		m.logger.Error("Failed to unmarshal job message",
			slog.String("project", project),
			slog.String("message_id", msg.MessageId),
			slog.String("error", err.Error()),
		)
		// Nack the message so it might be retried or dead-lettered
		_ = ackNacker.Nack(false) // requeue=false
		ch.Close()                // Close channel after Nack
		return nil, nil, fmt.Errorf("failed to parse job message: %w", err)
	}

	// Convert JobMessage to TestJob
	testJob := &models.TestJob{
		ID:         jobMsg.ID,
		Project:    jobMsg.Project,
		Details:    jobMsg.Details,
		Priority:   jobMsg.Priority,
		EnqueuedAt: jobMsg.EnqueuedAt,
		Status:     models.StatusRunning, // Assume status becomes Running upon dequeue
	}

	m.logger.Info("Dequeued job",
		slog.String("job_id", testJob.ID),
		slog.String("project", testJob.Project),
		slog.Uint64("priority", uint64(testJob.Priority)),
	)

	// Return the job and the AckNacker interface (which holds the channel open)
	return testJob, ackNacker, nil
}

// GetQueueSize attempts to get the current message count in a queue using a temporary channel.
func (m *RabbitMQManager) GetQueueSize(project string) (int, error) {
	queueName := project

	// Get a temporary channel for this operation
	ch, err := m.conn.Channel()
	if err != nil {
		// Check if the error is due to the underlying connection being closed
		if m.conn.IsClosed() {
			m.logger.Error("Cannot get queue size, connection is closed", slog.String("project", project))
			return 0, fmt.Errorf("connection is not open") // Return specific error maybe?
		}
		return 0, fmt.Errorf("failed to open temporary channel for queue size check: %w", err)
	}
	defer ch.Close() // Ensure temporary channel is closed

	// Use QueueDeclarePassive to get queue info without modifying it
	args := amqp.Table{"x-max-priority": int32(maxPriority)} // Must match original args
	q, err := ch.QueueDeclarePassive(
		queueName, // name
		true,      // durable
		false,     // delete when unused
		false,     // exclusive
		false,     // no-wait
		args,
	)
	if err != nil {
		// If queue doesn't exist, passive declare fails. Treat size as 0.
		if amqpErr, ok := err.(*amqp.Error); ok {
			if amqpErr.Code == amqp.NotFound {
				return 0, nil // Queue not found is size 0
			}
			// Return the original AMQP error if it's channel/connection related
			if amqpErr.Code == amqp.ChannelError || amqpErr.Code == amqp.ConnectionForced {
				return 0, fmt.Errorf("channel/connection error during passive declare: %w", err)
			}
		}
		// Return generic error for other passive declare issues
		return 0, fmt.Errorf("failed to passively declare queue '%s' to get size: %w", queueName, err)
	}
	return q.Messages, nil
}
