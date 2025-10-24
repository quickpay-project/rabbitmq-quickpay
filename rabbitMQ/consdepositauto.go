package rabbitmqconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type DepositautoRequest struct {
	Amount          float64 `json:"amount"`
	MID             string  `json:"mid"`
	CustomerOrderID string  `json:"customer_order_id"`
	CallbackURL     string  `json:"callback_url"`
}

type DepositautoResponse struct {
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

// ===== INSERT LOG =====
func insertDepositautoLog(
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
INSERT INTO depositauto_logs 
(queue_name, message_body, headers, http_status, http_response_body, status, attempts, error_message, api_transaction_id, created_at, updated_at)
VALUES ($1, $2::jsonb, $3::jsonb, $4, $5, $6, $7, $8, $9, now(), now())
RETURNING id;`

	var id int64
	err := db.QueryRow(query, queueName, msgJSON, headersJSON, httpStatus, httpRespBody, statusStr, attempts, errMsg, txnID).Scan(&id)
	return id, err
}

// ===== CALL EXTERNAL API =====
// ✅ คืนค่า URL แบบสุ่มตาม Group จาก header
func getRandomDepositautoURLByGroup(headers map[string]interface{}) (string, error) {
	// ✅ ดึงค่า group จาก header
	var group string
	if v, ok := headers["Group"]; ok {
		switch g := v.(type) {
		case string:
			group = g
		case []byte:
			group = string(g)
		}
	}
	if group == "" {
		return "", errors.New("missing Group header")
	}

	// ✅ สร้าง key env เช่น DEPOSITAUTO_URL_GROUP1
	envKey := "DEPOSIT_AUTO_URL_GROUP" + group
	envValue := os.Getenv(envKey)
	if envValue == "" {
		return "", fmt.Errorf("no URLs found for group %s", group)
	}

	// ✅ แยกเป็น slice
	urls := strings.Split(envValue, ",")
	if len(urls) == 0 {
		return "", fmt.Errorf("no valid URLs in %s", envKey)
	}

	// ✅ random URL
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return urls[r.Intn(len(urls))], nil
}

// ✅ ฟังก์ชันยิง API deposit
func sendToExternalDepositautoAPI(data []byte, headers map[string]interface{}) (int, string, string, []string, error) {
	apiURL, err := getRandomDepositautoURLByGroup(headers)
	if err != nil {
		return 0, "", "", nil, err
	}

	log.Printf("🌐 Deposit AUTO API URL (Group): %s", apiURL)

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(data))
	if err != nil {
		return 0, "", "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	authHeader := ""
	if v, ok := headers["Authorization"]; ok {
		if token, ok := v.(string); ok {
			authHeader = token
		} else if b, ok := v.([]byte); ok {
			authHeader = string(b)
		}
	}
	req.Header.Set("Authorization", authHeader)

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", "", nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", "", nil, err
	}

	var depositResp DepositautoResponse
	if err := json.Unmarshal(respBytes, &depositResp); err != nil {
		return resp.StatusCode, string(respBytes), "", nil, fmt.Errorf("cannot parse response: %w", err)
	}

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
		return resp.StatusCode, string(respBytes), firstTxnID, txnIDs, err
	}

	return resp.StatusCode, string(respJSON), firstTxnID, txnIDs, nil
}

// ===== RPC CONSUMER =====
// ConnectMQ / CloseMQ ควรมีในโปรเจกต์ของคุณอยู่แล้ว
func (r *RabbitDepositautoMQ) ConsdepositautoRPC() {
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
				processDepositautoMessage(d, ch, r.QueueName)
			}
		}(i)
	}

	wg.Wait()
}

func processDepositautoMessage(d amqp.Delivery, ch *amqp.Channel, queueName string) {
	headers := map[string]interface{}{}
	for k, v := range d.Headers {
		headers[k] = v
	}

	httpStatus, respBody, firstTxnID, txnIDs, sendErr := sendToExternalDepositautoAPI(d.Body, headers)

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

	_, _ = insertDepositautoLog(queueName, d.Body, headers, httpStatus, respBody, status, 1, errMsg, firstTxnID)

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
