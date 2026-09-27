package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validBase() map[string]string {
	return map[string]string{
		"RABBITMQ_URL": "amqp://guest:guest@localhost:5672/",
		"DATABASE_URL": "host=localhost user=postgres dbname=mqv2",
		"QUEUE_PREFIX": "v2.",
		"ALLOWED_IPS":  "1.2.3.4",
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(env(validBase()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != "4000" {
		t.Errorf("Port = %q, want 4000", c.Port)
	}
	if c.ReconcileInterval != 30*time.Second {
		t.Errorf("ReconcileInterval = %v, want 30s", c.ReconcileInterval)
	}
	if c.RPCChannelPool != 4 {
		t.Errorf("RPCChannelPool = %d, want 4", c.RPCChannelPool)
	}
	if c.GracefulTimeout != 45*time.Second {
		t.Errorf("GracefulTimeout = %v, want 45s", c.GracefulTimeout)
	}
	if c.TrustedProxyCount != 1 {
		t.Errorf("TrustedProxyCount = %d, want 1", c.TrustedProxyCount)
	}
	if !c.MigrateOnStart {
		t.Error("MigrateOnStart ต้องเป็น true โดย default")
	}
}

func TestLoadRequiresEachMandatoryVarAndNamesIt(t *testing.T) {
	for _, key := range []string{"RABBITMQ_URL", "DATABASE_URL", "QUEUE_PREFIX"} {
		m := validBase()
		delete(m, key)
		_, err := Load(env(m))
		if err == nil {
			t.Fatalf("ไม่มี %s แล้วต้อง error", key)
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error ต้องบอกชื่อตัวแปร %s แต่ได้ %q", key, err.Error())
		}
	}
}

func TestQueuePrefixWhitespaceOnlyIsRejected(t *testing.T) {
	m := validBase()
	m["QUEUE_PREFIX"] = "   "
	if _, err := Load(env(m)); err == nil {
		t.Fatal("QUEUE_PREFIX ที่มีแต่ช่องว่างต้องไม่ผ่าน ไม่งั้นจะชน queue ของระบบเก่า")
	}
}

func TestAllowedIPsEmptyMeansDenyAll(t *testing.T) {
	m := validBase()
	m["ALLOWED_IPS"] = ""
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AllowAllIPs {
		t.Error("ค่าว่างต้องไม่ใช่การเปิดให้ทุกคน")
	}
	if len(c.AllowedIPs) != 0 {
		t.Errorf("AllowedIPs = %v, want ว่าง", c.AllowedIPs)
	}
}

func TestAllowedIPsStarMeansAllowAll(t *testing.T) {
	m := validBase()
	m["ALLOWED_IPS"] = "*"
	c, _ := Load(env(m))
	if !c.AllowAllIPs {
		t.Error(`ALLOWED_IPS="*" ต้องเปิดให้ทุกคน`)
	}
}

func TestAllowedIPsTrimsSpaces(t *testing.T) {
	m := validBase()
	m["ALLOWED_IPS"] = " 1.2.3.4 , 5.6.7.8 ,, "
	c, _ := Load(env(m))
	want := []string{"1.2.3.4", "5.6.7.8"}
	if len(c.AllowedIPs) != len(want) {
		t.Fatalf("AllowedIPs = %v, want %v", c.AllowedIPs, want)
	}
	for i := range want {
		if c.AllowedIPs[i] != want[i] {
			t.Errorf("AllowedIPs[%d] = %q, want %q", i, c.AllowedIPs[i], want[i])
		}
	}
}

func TestInvalidDurationIsRejected(t *testing.T) {
	m := validBase()
	m["RECONCILE_INTERVAL"] = "สามสิบวิ"
	_, err := Load(env(m))
	if err == nil || !strings.Contains(err.Error(), "RECONCILE_INTERVAL") {
		t.Fatalf("ต้อง error พร้อมบอกชื่อ RECONCILE_INTERVAL แต่ได้ %v", err)
	}
}

func TestNonPositiveNumbersRejected(t *testing.T) {
	for key, bad := range map[string]string{
		"RPC_CHANNEL_POOL":    "0",
		"TRUSTED_PROXY_COUNT": "-1",
	} {
		m := validBase()
		m[key] = bad
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s=%s ต้องไม่ผ่าน", key, bad)
		}
	}
}

func TestMigrateOnStartCanBeDisabled(t *testing.T) {
	m := validBase()
	m["MIGRATE_ON_START"] = "false"
	c, _ := Load(env(m))
	if c.MigrateOnStart {
		t.Error("MIGRATE_ON_START=false ต้องปิด migration")
	}
}
