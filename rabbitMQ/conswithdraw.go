package rabbitmqconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
	amqp "github.com/rabbitmq/amqp091-go"
)

type WithdrawRequest struct {
	CustomerOrderID string  `json:"customer_order_id"`
	MID             string  `json:"mid"`
	AccountNumber   string  `json:"account_number"`
	BankCode        string  `json:"bank_code"`
	BankName        string  `json:"bank_name"`
	AccountName     string  `json:"account_name"`
	Amount          float64 `json:"amount"`
	Cost            float64 `json:"cost"`
	WithdrawType    string  `json:"withdraw_type"`
	Settlement      string  `json:"settlement"`
	NonFundedType   string  `json:"non_funded_type"`
	CallbackURL     string  `json:"callback_url"`
	Remark          string  `json:"remark"`
}

type WithdrawResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Order struct {
			OperatorOrderID string      `json:"operator_order_id"`
			CustomerOrderID string      `json:"customer_order_id"`
			Amount          float64     `json:"amount"`
			AccountNumber   string      `json:"account_number"`
			AccountName     string      `json:"account_name"`
			BankCode        string      `json:"bank_code"`
			BankName        string      `json:"bank_name"`
			Cost            float64     `json:"Cost"`
			WithdrawType    string      `json:"withdraw_type"`
			TotalOrder      int         `json:"total_order"`
			WithdrawDetails interface{} `json:"withdraw_details"`
		} `json:"order"`
		Details []struct {
			TransactionID string  `json:"transaction_id"`
			Amount        float64 `json:"amount"`
			CreatedAt     string  `json:"created_at"`
			WithdrawID    string  `json:"withdraw_id"`
		} `json:"details"`
	} `json:"data"`
}

// ===== INSERT LOG =====
func insertWithdrawLog(
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
INSERT INTO withdraw_logs 
(queue_name, message_body, headers, http_status, http_response_body, status, attempts, error_message, api_transaction_id, created_at, updated_at)
VALUES ($1, $2::jsonb, $3::jsonb, $4, $5, $6, $7, $8, $9, now(), now())
RETURNING id;`

	var id int64
	err := db.QueryRow(query, queueName, msgJSON, headersJSON, httpStatus, httpRespBody, statusStr, attempts, errMsg, txnID).Scan(&id)
	return id, err
}

// ===== CALL EXTERNAL API =====
// return random URL from env WITHDRAW_URL_GROUP
func getRandomWithdrawURL(_ map[string]interface{}) (string, error) {
	envValue := os.Getenv("WITHDRAW_URL_GROUP")
	if envValue == "" {
		return "", errors.New("WITHDRAW_URL_GROUP not set")
	}

	urls := strings.Split(envValue, ",")
	if len(urls) == 0 {
		return "", errors.New("no valid URLs in WITHDRAW_URL_GROUP")
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return strings.TrimSpace(urls[r.Intn(len(urls))]), nil
}

func sendToExternalWithdrawAPI(data []byte, headers map[string]interface{}) (int, string, string, []string, error) {
	apiURL, err := getRandomWithdrawURL(headers)
	if err != nil {
		return 0, "", "", nil, err
	}

	log.Printf("🌐 Withdraw API URL: %s", apiURL)

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

	var withdrawResp WithdrawResponse
	if err := json.Unmarshal(respBytes, &withdrawResp); err != nil {
		log.Printf("❌ Cannot parse response: %v", err)
		return resp.StatusCode, string(respBytes), "", nil, err
	}

	// extract txn IDs
	txnIDs := make([]string, 0, len(withdrawResp.Data.Details))
	for _, d := range withdrawResp.Data.Details {
		if d.TransactionID != "" {
			txnIDs = append(txnIDs, d.TransactionID)
		}
	}
	firstTxnID := ""
	if len(txnIDs) > 0 {
		firstTxnID = txnIDs[0]
	}

	respJSON, err := json.Marshal(withdrawResp)
	if err != nil {
		return resp.StatusCode, string(respBytes), "", nil, err
	}

	return resp.StatusCode, string(respJSON), firstTxnID, txnIDs, nil
}

// ===== RPC CONSUMER =====

// ConnectMQ / CloseMQ ควรมีในโปรเจกต์ของคุณอยู่แล้ว
func (r *RabbitWithdrawMQ) ConswithdrawRPC() {
	if err := InitDB(); err != nil {
		log.Fatalf("❌ InitDB failed: %v", err)
	}
	defer db.Close()

	conn, ch := ConnectMQ()
	defer CloseMQ(conn, ch)

	// ตั้ง prefetch count (เช่น 10 ข้อความต่อ worker)
	if err := ch.Qos(10, 0, false); err != nil {
		log.Fatalf("❌ QoS set failed: %v", err)
	}

	q, err := ch.QueueDeclare(r.QueueName, false, false, false, false, nil)
	if err != nil {
		log.Fatalf("❌ Queue declare error: %v", err)
	}

	msgs, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("❌ Consume error: %v", err)
	}

	workerCountStr := os.Getenv("WITHDRAW_LIMIT")
	workerCount, err := strconv.Atoi(workerCountStr)
	if err != nil {
		log.Fatalf("❌ invalid WITHDRAW_LIMIT: %v", err)
	}

	for i := 0; i < workerCount; i++ {
		go func(workerID int) {
			for d := range msgs {
				headers := map[string]interface{}{}
				for k, v := range d.Headers {
					headers[k] = v
				}

				httpStatus, respBody, firstTxnID, txnIDs, sendErr := sendToExternalWithdrawAPI(d.Body, headers)

				log.Printf("HTTP Status: %d", httpStatus)
				log.Printf("Transaction ID: %s", firstTxnID)
				log.Printf("Transaction IDs: %v", txnIDs)
				//log.Printf("Body length: %d", len(respBody))

				status := "sent"
				errMsg := ""
				var rpcResponse []byte

				if httpStatus == 400 {
					message := "เกิดข้อผิดพลาดถอนเงิน"
					respBytes := []byte(respBody)
					if json.Valid(respBytes) {
						var respMap map[string]interface{}
						if err := json.Unmarshal(respBytes, &respMap); err == nil {
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
						"data":    nil,
					})
				} else if sendErr != nil || httpStatus >= 500 {
					status = "failed"
					if sendErr != nil {
						errMsg = sendErr.Error()
					}
					rpcResponse, _ = json.Marshal(map[string]interface{}{
						"code":    httpStatus,
						"message": errMsg,
						"data":    nil,
					})
				} else {
					rpcResponse = []byte(respBody)
				}

				_, _ = insertWithdrawLog(r.QueueName, d.Body, headers, httpStatus, respBody, status, 1, errMsg, firstTxnID)

				if d.ReplyTo != "" {
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

				d.Ack(false)
			}
		}(i + 1)
	}

	// กัน main goroutine ออกจากโปรแกรม
	select {}
}
