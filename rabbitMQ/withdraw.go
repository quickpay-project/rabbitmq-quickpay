package rabbitmqconnect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type RabbitWithdrawMQ struct {
	Body      string
	QueueName string
	Headers   map[string]string
}

// genCorrelationID สร้าง UUID/Random string
func genCorrelationID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ========== RPC CLIENT ==========
func (r *RabbitWithdrawMQ) WithdrawRPC() ([]byte, error) {
	conn, ch := ConnectMQ()
	defer CloseMQ(conn, ch)

	replyQueue, err := ch.QueueDeclare(
		"", false, true, true, false, nil,
	)
	if err != nil {
		return nil, err
	}

	msgs, err := ch.Consume(replyQueue.Name, "", true, false, false, false, nil)
	if err != nil {
		return nil, err
	}

	corrID := genCorrelationID()

	headers := amqp.Table{}
	for key, value := range r.Headers {
		headers[key] = value
	}

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
			ReplyTo:       replyQueue.Name,
		})
	if err != nil {
		return nil, err
	}

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
