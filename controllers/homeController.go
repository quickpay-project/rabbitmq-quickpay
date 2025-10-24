package controller

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	rabbitmqconnect "github.com/celalsahinaltinisik/rabbitMQ"
)

type Functions struct{}

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

func isWhitelistedIP(r *http.Request) bool {
	wl := os.Getenv("WISHLIST_IP")
	if wl == "" {
		return false
	}

	allowedIPs := strings.Split(wl, ",")

	// ✅ ดึงค่า IP
	ip := r.Header.Get("X-Forwarded-For")
	if ip != "" {
		// ตัดเอาเฉพาะตัวแรก
		ip = strings.Split(ip, ",")[0]
	} else {
		ip = r.RemoteAddr
		if strings.Contains(ip, ":") {
			ip = strings.Split(ip, ":")[0]
		}
	}

	ip = strings.TrimSpace(ip)
	log.Printf("🌐 Client IP: %s", ip)

	for _, allow := range allowedIPs {
		if strings.TrimSpace(allow) == ip {
			return true
		}
	}
	return false
}

func (m Functions) Home(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "home")
}

func (m Functions) Online(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "enable service mq")
}

func (m Functions) Conswithdraw(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "Consume withdraw")

	// อ่าน Header
	headers := make(map[string]string)
	for key, values := range r.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	rabbit := rabbitmqconnect.RabbitWithdrawMQ{QueueName: "withdraw", Headers: headers}
	rabbit.ConswithdrawRPC() // ใช้ RPC consumer
}

func (m Functions) Conswithdrawauto(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "Consume withdraw auto")

	// อ่าน Header
	headers := make(map[string]string)
	for key, values := range r.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	rabbit := rabbitmqconnect.RabbitWithdrawautoMQ{QueueName: "withdrawauto", Headers: headers}
	rabbit.ConswithdrawautoRPC() // ใช้ RPC consumer
}

func (m Functions) Consdeposit(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "Consume deposit")

	// อ่าน Header
	headers := make(map[string]string)
	for key, values := range r.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	rabbit := rabbitmqconnect.RabbitDepositMQ{QueueName: "deposit", Headers: headers}
	rabbit.ConsdepositRPC() // ใช้ RPC consumer
}

func (m Functions) Consdepositauto(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "Consume deposit")

	// อ่าน Header
	headers := make(map[string]string)
	for key, values := range r.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	rabbit := rabbitmqconnect.RabbitDepositautoMQ{QueueName: "depositauto", Headers: headers}
	rabbit.ConsdepositautoRPC() // ใช้ RPC consumer
}

func (m Functions) Withdraw(w http.ResponseWriter, r *http.Request) {
	// ✅ check IP ก่อน
	// if !isWhitelistedIP(r) {
	// 	http.Error(w, "Forbidden: IP not allowed", http.StatusForbidden)
	// 	return
	// }

	body, _ := io.ReadAll(r.Body)
	headers := make(map[string]string)
	for key, values := range r.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	rabbit := rabbitmqconnect.RabbitWithdrawMQ{
		Body:      string(body),
		QueueName: "withdraw",
		Headers:   headers,
	}

	response, err := rabbit.WithdrawRPC()
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

func (m Functions) Withdrawauto(w http.ResponseWriter, r *http.Request) {
	// ✅ check IP ก่อน
	// if !isWhitelistedIP(r) {
	// 	http.Error(w, "Forbidden: IP not allowed", http.StatusForbidden)
	// 	return
	// }

	body, _ := io.ReadAll(r.Body)
	headers := make(map[string]string)
	for key, values := range r.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	rabbit := rabbitmqconnect.RabbitWithdrawautoMQ{
		Body:      string(body),
		QueueName: "withdrawauto",
		Headers:   headers,
	}

	response, err := rabbit.WithdrawautoRPC()
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

func (m Functions) Deposit(w http.ResponseWriter, r *http.Request) {
	// ✅ check IP ก่อน
	// if !isWhitelistedIP(r) {
	// 	http.Error(w, "Forbidden: IP not allowed", http.StatusForbidden)
	// 	return
	// }

	body, _ := io.ReadAll(r.Body)
	headers := make(map[string]string)
	for key, values := range r.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	rabbit := rabbitmqconnect.RabbitDepositMQ{
		Body:      string(body),
		QueueName: "deposit",
		Headers:   headers,
	}

	response, err := rabbit.DepositRPC()
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

func (m Functions) Depositauto(w http.ResponseWriter, r *http.Request) {
	// ✅ check IP ก่อน
	// if !isWhitelistedIP(r) {
	// 	http.Error(w, "Forbidden: IP not allowed", http.StatusForbidden)
	// 	return
	// }

	body, _ := io.ReadAll(r.Body)
	headers := make(map[string]string)
	for key, values := range r.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}

	rabbit := rabbitmqconnect.RabbitDepositautoMQ{
		Body:      string(body),
		QueueName: "depositauto",
		Headers:   headers,
	}

	response, err := rabbit.DepositautoRPC()
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}
