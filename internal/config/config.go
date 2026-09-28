package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	RabbitMQURL string
	DatabaseURL string
	QueuePrefix string

	AllowedIPs  []string
	AllowAllIPs bool

	Port              string
	ReconcileInterval time.Duration
	RPCChannelPool    int
	GracefulTimeout   time.Duration
	TrustedProxyCount int
	// ClientIPHeader ว่าง = นับ hop ตาม TrustedProxyCount เหมือนเดิม
	// ตั้งไว้ = อ่าน IP จาก header นั้นตรง ๆ เช่น CF-Connecting-IP ของ Cloudflare
	ClientIPHeader string
	MigrateOnStart bool
}

// Load อ่าน config จาก getenv แล้วคืน error ทันทีถ้าตั้งผิด
// ตั้งใจให้ process ตายตั้งแต่ตอน start ไม่ใช่ไปพังตอน runtime แบบระบบเก่า
// ที่ strconv.Atoi("") ล้มกลางทางแล้วฆ่าทั้ง container
func Load(getenv func(string) string) (*Config, error) {
	c := &Config{}
	var errs []string

	req := func(key string) string {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			errs = append(errs, fmt.Sprintf("%s ต้องตั้งค่า", key))
		}
		return v
	}

	c.RabbitMQURL = req("RABBITMQ_URL")
	c.DatabaseURL = req("DATABASE_URL")
	// QUEUE_PREFIX กันไม่ให้ service ใหม่ไปแย่งกิน queue ของระบบเก่าที่ใช้ broker เดียวกัน
	c.QueuePrefix = req("QUEUE_PREFIX")

	raw := strings.TrimSpace(getenv("ALLOWED_IPS"))
	if raw == "*" {
		c.AllowAllIPs = true
	} else {
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				c.AllowedIPs = append(c.AllowedIPs, p)
			}
		}
	}

	c.Port = str(getenv, "PORT", "4000")
	c.ClientIPHeader = strings.TrimSpace(getenv("CLIENT_IP_HEADER"))
	c.MigrateOnStart = boolean(getenv, "MIGRATE_ON_START", true)

	c.ReconcileInterval = duration(getenv, "RECONCILE_INTERVAL", 30*time.Second, &errs)
	c.GracefulTimeout = duration(getenv, "GRACEFUL_TIMEOUT", 45*time.Second, &errs)
	c.RPCChannelPool = positiveInt(getenv, "RPC_CHANNEL_POOL", 4, &errs)
	c.TrustedProxyCount = nonNegativeInt(getenv, "TRUSTED_PROXY_COUNT", 1, &errs)

	if len(errs) > 0 {
		return nil, fmt.Errorf("config ไม่ถูกต้อง: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

func str(getenv func(string) string, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}

func boolean(getenv func(string) string, key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(getenv(key)))
	if v == "" {
		return def
	}
	return v != "false" && v != "0" && v != "no"
}

func duration(getenv func(string) string, key string, def time.Duration, errs *[]string) time.Duration {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s อ่านไม่ได้: %v", key, err))
		return def
	}
	if d <= 0 {
		*errs = append(*errs, fmt.Sprintf("%s ต้องมากกว่า 0", key))
		return def
	}
	return d
}

func positiveInt(getenv func(string) string, key string, def int, errs *[]string) int {
	n, ok := parseInt(getenv, key, def, errs)
	if ok && n < 1 {
		*errs = append(*errs, fmt.Sprintf("%s ต้องมากกว่า 0", key))
	}
	return n
}

func nonNegativeInt(getenv func(string) string, key string, def int, errs *[]string) int {
	n, ok := parseInt(getenv, key, def, errs)
	if ok && n < 0 {
		*errs = append(*errs, fmt.Sprintf("%s ต้องไม่ติดลบ", key))
	}
	return n
}

func parseInt(getenv func(string) string, key string, def int, errs *[]string) (int, bool) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s อ่านไม่ได้: %v", key, err))
		return def, false
	}
	return n, true
}
