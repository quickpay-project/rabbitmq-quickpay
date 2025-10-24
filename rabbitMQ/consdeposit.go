package rabbitmqconnect

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	amqp "github.com/rabbitmq/amqp091-go"
)

var db *sql.DB

type DepositRequest struct {
	Amount          float64 `json:"amount"`
	MID             string  `json:"mid"`
	CustomerOrderID string  `json:"customer_order_id"`
	CallbackURL     string  `json:"callback_url"`
}

type DepositResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Order struct {
			OperatorOrderID string      `json:"operator_order_id"`
			CustomerOrderID string      `json:"customer_order_id"`
			QRType          string      `json:"qr_type"`
			Amount          float64     `json:"amount"`
			AccountNumber   string      `json:"account_number"`
			AccountName     string      `json:"account_name"`
			TotalQRCode     int         `json:"total_qr_code"`
			QRDetails       interface{} `json:"qr_details"`
			BankCode        string      `json:"bank_code"`
			CallbackURL     interface{} `json:"callback_url"`
		} `json:"order"`
		Details []struct {
			TransactionID   string      `json:"transaction_id"`
			QRString        string      `json:"qr_string"`
			Amount          float64     `json:"amount"`
			NetAmount       float64     `json:"net_amount"`
			CreatedAt       string      `json:"created_at"`
			ExpiredAt       string      `json:"expired_at"`
			ImageURL        string      `json:"image_url"`
			BankCode        string      `json:"bank_code"`
			AccountName     string      `json:"account_name"`
			AccountNumber   string      `json:"account_number"`
			CustomerOrderID interface{} `json:"customer_order_id"`
			UpdatedAt       interface{} `json:"updated_at"`
			MdrAmount       interface{} `json:"mdr_amount"`
			FeeAmount       interface{} `json:"fee_amount"`
			VATAmount       interface{} `json:"vat_amount"`
			WHTAmount       interface{} `json:"wht_amount"`
		} `json:"details"`
	} `json:"data"`
}

// ===== DB INIT =====
func InitDB() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}
	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Minute * 10)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

// ===== INSERT LOG =====
func insertDepositLog(
	queueName string,
	body []byte,
	headers map[string]interface{},
	httpStatus int,
	httpRespBody string,
	statusStr string,
	attempts int,
	errMsg string,
	txnID string,
) (int64, error) {

	if db == nil {
		return 0, errors.New("db not initialized")
	}

	headersJSON, _ := json.Marshal(headers)
	var msgJSON json.RawMessage
	if json.Valid(body) {
		msgJSON = body
	} else {
		wrapped, _ := json.Marshal(map[string]string{"raw": string(body)})
		msgJSON = wrapped
	}

	query := `
INSERT INTO deposit_logs 
(queue_name, message_body, headers, http_status, http_response_body, status, attempts, error_message, api_transaction_id, created_at, updated_at)
VALUES ($1, $2::jsonb, $3::jsonb, $4, $5, $6, $7, $8, $9, now(), now())
RETURNING id;`

	var id int64
	err := db.QueryRow(query, queueName, msgJSON, headersJSON, httpStatus, httpRespBody, statusStr, attempts, errMsg, txnID).Scan(&id)
	return id, err
}

// ===== CALL EXTERNAL API =====
// return random URL from env DEPOSIT_URL_GROUP
func getRandomDepositURL(_ map[string]interface{}) (string, error) {
	envValue := os.Getenv("DEPOSIT_URL_GROUP")
	if envValue == "" {
		return "", errors.New("DEPOSIT_URL_GROUP not set")
	}

	urls := strings.Split(envValue, ",")
	if len(urls) == 0 {
		return "", errors.New("no valid URLs in DEPOSIT_URL_GROUP")
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return strings.TrimSpace(urls[r.Intn(len(urls))]), nil
}

func sendToExternalDepositAPI(data []byte, headers map[string]interface{}) (int, string, string, []string, error) {
	apiURL, err := getRandomDepositURL(headers)
	if err != nil {
		return 0, "", "", nil, err
	}

	log.Printf("🌐 Deposit API URL: %s", apiURL)

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(data))
	if err != nil {
		return 0, "", "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// set Authorization header if exists
	if v, ok := headers["Authorization"]; ok {
		if token, ok := v.(string); ok {
			req.Header.Set("Authorization", token)
		} else if b, ok := v.([]byte); ok {
			req.Header.Set("Authorization", string(b))
		}
	}

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("❌ Request failed: %v", err)
		return 0, "", "", nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", "", nil, err
	}

	var depositResp DepositResponse
	if err := json.Unmarshal(respBytes, &depositResp); err != nil {
		log.Printf("❌ Cannot parse response: %v", err)
		return resp.StatusCode, string(respBytes), "", nil, err
	}

	// extract txn IDs
	txnIDs := make([]string, 0, len(depositResp.Data.Details))
	for _, d := range depositResp.Data.Details {
		if d.TransactionID != "" {
			txnIDs = append(txnIDs, d.TransactionID)
		}
	}
	firstTxnID := ""
	if len(txnIDs) > 0 {
		firstTxnID = txnIDs[0]
	}

	respJSON, err := json.Marshal(depositResp)
	if err != nil {
		return resp.StatusCode, string(respBytes), "", nil, err
	}

	return resp.StatusCode, string(respJSON), firstTxnID, txnIDs, nil
}

// ===== RPC CONSUMER =====
// ConnectMQ / CloseMQ ควรมีในโปรเจกต์ของคุณอยู่แล้ว
func (r *RabbitDepositMQ) ConsdepositRPC() {
	if err := InitDB(); err != nil {
		log.Fatalf("❌ InitDB failed: %v", err)
	}
	defer db.Close()

	conn, ch := ConnectMQ()
	defer CloseMQ(conn, ch)

	q, err := ch.QueueDeclare(r.QueueName, false, false, false, false, nil)
	if err != nil {
		log.Fatalf("❌ Queue declare error: %v", err)
	}

	workerCountStr := os.Getenv("DEPOSIT_LIMIT")
	workerCount, err := strconv.Atoi(workerCountStr)
	if err != nil {
		log.Fatalf("❌ invalid DEPOSIT_LIMIT: %v", err)
	}

	if err := ch.Qos(workerCount, 0, false); err != nil {
		log.Fatalf("❌ QoS set error: %v", err)
	}

	msgs, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("❌ Consume error: %v", err)
	}

	log.Printf("[*] Waiting for RPC requests on queue: %s", q.Name)

	// ✅ สร้าง worker pool
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for d := range msgs {
				processDepositMessage(d, ch, r.QueueName)
			}
		}(i)
	}

	wg.Wait()
}

func processDepositMessage(d amqp.Delivery, ch *amqp.Channel, queueName string) {
	headers := map[string]interface{}{}
	for k, v := range d.Headers {
		headers[k] = v
	}

	httpStatus, respBody, firstTxnID, txnIDs, sendErr := sendToExternalDepositAPI(d.Body, headers)

	log.Printf("HTTP Status: %d", httpStatus)
	log.Printf("Transaction IDs: %v", txnIDs)

	status := "sent"
	errMsg := ""
	var rpcResponse []byte

	if httpStatus == 400 {
		message := "เกิดข้อผิดพลาดฝากเงิน"
		if json.Valid([]byte(respBody)) {
			var respMap map[string]interface{}
			if err := json.Unmarshal([]byte(respBody), &respMap); err == nil {
				if msg, ok := respMap["message"].(string); ok && msg != "" {
					message = msg
				}
			}
		}
		errMsg = message
		status = "failed"
		rpcResponse, _ = json.Marshal(map[string]interface{}{
			"code":    400,
			"message": message,
		})
	} else if sendErr != nil || httpStatus >= 500 {
		status = "failed"
		if sendErr != nil {
			errMsg = sendErr.Error()
		}
	}

	_, _ = insertDepositLog(queueName, d.Body, headers, httpStatus, respBody, status, 1, errMsg, firstTxnID)

	if d.ReplyTo != "" {
		if rpcResponse == nil {
			if json.Valid([]byte(respBody)) {
				rpcResponse = []byte(respBody)
			} else {
				rpcResponse, _ = json.Marshal(map[string]interface{}{
					"status":  httpStatus,
					"message": errMsg,
				})
			}
		}

		_ = ch.PublishWithContext(context.Background(),
			"",
			d.ReplyTo,
			false,
			false,
			amqp.Publishing{
				ContentType:   "application/json",
				CorrelationId: d.CorrelationId,
				Body:          rpcResponse,
			})
	}

	d.Ack(false) // ✅ Ack หลังจากประมวลผลเสร็จ
}
