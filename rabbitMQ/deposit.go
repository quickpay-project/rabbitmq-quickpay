package rabbitmqconnect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type RabbitDepositMQ struct {
	Body      string
	QueueName string
	Headers   map[string]string
}

// genCorrelationID สร้าง UUID/Random string
func genCorrelationDepositID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ========== RPC CLIENT ==========
func (r *RabbitDepositMQ) DepositRPC() ([]byte, error) {
	conn, ch := ConnectMQ()
	defer CloseMQ(conn, ch)

	// ✅ ใช้ Direct Reply-To (ไม่ต้อง QueueDeclare)
	replyQueue := "amq.rabbitmq.reply-to"

	msgs, err := ch.Consume(
		replyQueue,
		"",
		true,  // auto-ack
		true,  // exclusive
		false, // no-local
		false, // no-wait
		nil,
	)
	if err != nil {
		return nil, err
	}

	corrID := genCorrelationDepositID()

	headers := amqp.Table{}
	for key, value := range r.Headers {
		headers[key] = value
	}

	// ✅ Publish context รอได้นานขึ้นหน่อย
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = ch.PublishWithContext(ctx,
		"",
		r.QueueName,
		false,
		false,
		amqp.Publishing{
			ContentType:   "application/json",
			Body:          []byte(r.Body),
			Headers:       headers,
			CorrelationId: corrID,
			ReplyTo:       replyQueue,
		})
	if err != nil {
		return nil, err
	}

	// ✅ ลด timeout ลงเพื่อให้เร็วขึ้น (เช่น 10s)
	timeout := time.After(90 * time.Second)

	for {
		select {
		case msg := <-msgs:
			if msg.CorrelationId == corrID {
				return msg.Body, nil
			}
		case <-timeout:
			return nil, errors.New("RPC timeout waiting for response")
		}
	}
}
