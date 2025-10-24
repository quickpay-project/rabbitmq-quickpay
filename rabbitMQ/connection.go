package rabbitmqconnect

import (
	"log"
	"os"

	errors "github.com/celalsahinaltinisik/exceptions"
	amqp "github.com/rabbitmq/amqp091-go"
)

func ConnectMQ() (*amqp.Connection, *amqp.Channel) {
	rabbitURL := os.Getenv("RABBITMQ_URL")
	if rabbitURL == "" {
		rabbitURL = "amqp://guest:guest@rabbitmq:5672/" // default ถ้าไม่มี ENV
	}

	conn, err := amqp.Dial(rabbitURL)
	errors.FailOnError(err, "Failed to connect to RabbitMQ")

	ch, err := conn.Channel()
	errors.FailOnError(err, "Failed to open a channel")

	log.Println("✅ Connected to RabbitMQ:", rabbitURL)
	return conn, ch
}

func CloseMQ(conn *amqp.Connection, channel *amqp.Channel) {
	if channel != nil {
		_ = channel.Close()
	}
	if conn != nil {
		_ = conn.Close()
	}
}
