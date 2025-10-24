# ใช้ Golang base image เล็กและรวดเร็ว
FROM golang:1.21-alpine

# ตั้ง working directory
WORKDIR /app

# คัดลอก go.mod และ go.sum เพื่อดาวน์โหลด dependencies ก่อน
COPY go.mod go.sum ./
RUN go mod download

# คัดลอกโค้ดทั้งหมด
COPY . .

# ให้ wait-for-rabbitmq.sh เป็น executable
RUN chmod +x wait-for-rabbitmq.sh

# build binary
RUN go build -o main .

# command รันไฟล์ wait-for-rabbitmq.sh แล้วค่อยรัน main
CMD ["./wait-for-rabbitmq.sh", "rabbitmq", "./main"]
