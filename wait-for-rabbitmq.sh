#!/bin/sh
# wait-for-rabbitmq.sh

set -e

host="${RABBITMQ_HOST:-rabbitmq}"
port="${RABBITMQ_PORT:-5672}"

echo "⏳ Waiting for RabbitMQ at $host:$port..."

while ! nc -z "$host" "$port"; do
  sleep 1
done

echo "✅ RabbitMQ is up - executing command"
exec "$@"
