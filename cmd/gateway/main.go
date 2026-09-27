package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/celalsahinaltinisik/internal/amqpx"
	"github.com/celalsahinaltinisik/internal/config"
	"github.com/celalsahinaltinisik/internal/flow"
	"github.com/celalsahinaltinisik/internal/forward"
	"github.com/celalsahinaltinisik/internal/httpapi"
	"github.com/celalsahinaltinisik/internal/model"
	"github.com/celalsahinaltinisik/internal/reconcile"
	"github.com/celalsahinaltinisik/internal/store"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// 1. config — ขาดอะไรตายตรงนี้พร้อมบอกชื่อตัวแปร
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		log.Fatalf("❌ %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2-3. DB + migration
	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("❌ เปิด DB ไม่ได้: %v", err)
	}
	defer db.Close()

	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := db.PingContext(pingCtx); err != nil {
		pingCancel()
		log.Fatalf("❌ ต่อ DB ไม่ได้: %v", err)
	}
	pingCancel()

	if cfg.MigrateOnStart {
		if err := store.Migrate(ctx, db); err != nil {
			log.Fatalf("❌ migration ล้มเหลว: %v", err)
		}
		log.Printf("✅ migration เรียบร้อย")
	}
	st := store.New(db)

	// 4. RabbitMQ
	mgr := amqpx.NewManager(cfg.RabbitMQURL)
	mgr.Logf = log.Printf
	defer mgr.Close()

	pool, err := amqpx.NewRPCPool(ctx, mgr, cfg.RPCChannelPool)
	if err != nil {
		log.Fatalf("❌ สร้าง RPC pool ไม่ได้: %v", err)
	}
	defer pool.Close()
	log.Printf("✅ ต่อ RabbitMQ แล้ว (pool %d ช่อง, prefix %q)", cfg.RPCChannelPool, cfg.QueuePrefix)

	// 5. registry + factory
	registry := flow.NewRegistry()
	fwd := forward.New()

	factory := func(spec model.GroupSpec) (*flow.Flow, error) {
		broker, err := amqpx.NewBroker(ctx, mgr)
		if err != nil {
			return nil, err
		}
		proc := flow.NewProcessor(fwd, st,
			func(ctx context.Context, replyTo string, pub amqp.Publishing) error {
				return broker.Publish(ctx, replyTo, pub)
			})
		proc.Logf = log.Printf

		return flow.New(flow.Options{
			Spec:    spec,
			Queue:   spec.QueueName(cfg.QueuePrefix),
			Broker:  broker,
			Process: proc.Handle,
			Logf:    log.Printf,
		}), nil
	}

	loop := &reconcile.Loop{
		Loader: st, Registry: registry, Factory: factory,
		Interval: cfg.ReconcileInterval,
		Drain:    cfg.GracefulTimeout,
		Logf:     log.Printf,
	}

	// 6. HTTP ขึ้นก่อน เพื่อให้ /healthz ตอบได้ระหว่างบูต
	handler := httpapi.New(httpapi.Options{
		Registry: registry, Logger: st, Caller: pool, Cfg: cfg,
		NewID: uuid.NewString, Logf: log.Printf,
	})
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("🌐 ฟังอยู่ที่ :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("❌ ListenAndServe: %v", err)
		}
	}()

	// 7. reconcile รอบแรกแบบ sync — flow ทุกตัวขึ้นตรงนี้ ไม่ต้อง curl อะไรทั้งนั้น
	if err := loop.Once(ctx); err != nil {
		log.Printf("⚠️  reconcile รอบแรกล้มเหลว: %v (จะลองใหม่ในรอบถัดไป)", err)
	}
	warnIfTimeoutsExceedGrace(ctx, st, cfg.GracefulTimeout)

	// 8. reconcile loop
	go loop.Run(ctx)

	// graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	sig := <-stop
	log.Printf("🛑 ได้รับสัญญาณ %v — เริ่ม graceful shutdown", sig)

	cancel() // หยุด reconcile loop

	// GRACEFUL_TIMEOUT คืองบ "ก้อนเดียว" ของ shutdown ทั้งหมด ไม่ใช่ก้อนละขั้น
	// เดิมให้ srv.Shutdown เต็มงบแล้วให้ drainAll เต็มงบอีกรอบ เวลาปิดแย่สุดจึงเป็นสองเท่า
	// (default 45s → 90s) ซึ่งเกิน stop timeout ที่ตั้งไว้ แล้วโดน SIGKILL กลาง drain
	// คือ pain point เดิมของระบบเก่าเป๊ะ ๆ ที่ service นี้มีไว้แก้
	shutdownDeadline := time.Now().Add(cfg.GracefulTimeout)

	shutdownCtx, shutdownCancel := context.WithDeadline(context.Background(), shutdownDeadline)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("⚠️  ปิด HTTP server: %v", err)
	}

	// drain ทุก flow ขนานกัน — Cancel ก่อนเสมอเพื่อไม่ให้ดูดงานใหม่เข้ามา
	// ได้เวลาเท่าที่เหลือจากงบเดียวกัน งบรวมจึงไม่เกิน GRACEFUL_TIMEOUT
	drainBudget := time.Until(shutdownDeadline)
	if drainBudget <= 0 {
		drainBudget = 0
		log.Printf("⚠️  ปิด HTTP server ใช้งบ GRACEFUL_TIMEOUT %v หมดแล้ว — drain ได้เวลา 0 "+
			"(ยังสั่ง Cancel consumer แต่ไม่รอของในมือ) ควรเพิ่ม GRACEFUL_TIMEOUT",
			cfg.GracefulTimeout)
	}
	drainAll(registry, drainBudget)
	log.Printf("👋 ปิดเรียบร้อย")
}

func drainAll(r *flow.Registry, timeout time.Duration) {
	snap := r.Snapshot()
	done := make(chan struct{}, len(snap))
	for id := range snap {
		go func(id string) {
			defer func() { done <- struct{}{} }()
			f, _, ok := r.Get(id)
			if !ok {
				return
			}
			if err := f.Drain(timeout); err != nil {
				log.Printf("⚠️  drain %s: %v", f.Spec().Name, err)
			}
		}(id)
	}
	for range snap {
		<-done
	}
}

// warnIfTimeoutsExceedGrace เตือนตั้งแต่ตอน start ถ้า config ทำให้ shutdown ไม่มีวันจบทัน
// ระบบเก่าต้องใช้ 30 วินาทีแต่ docker ให้ 10 วินาที จึงโดน SIGKILL ทุกครั้ง
func warnIfTimeoutsExceedGrace(ctx context.Context, st *store.Store, grace time.Duration) {
	groups, err := st.LoadGroups(ctx)
	if err != nil {
		return
	}
	for _, g := range groups {
		if g.RPCTimeout+10*time.Second > grace {
			log.Printf("⚠️  group %s: rpc_timeout %v + 10s เกิน GRACEFUL_TIMEOUT %v — "+
				"ตอน deploy งานที่ค้างจะถูกตัดกลางคัน", g.Name, g.RPCTimeout, grace)
		}
		urlCount := len(g.URLs)
		if urlCount == 0 {
			urlCount = 1
		}
		if g.UpstreamTimeout*time.Duration(urlCount) > g.RPCTimeout {
			log.Printf("⚠️  group %s: upstream_timeout %v × %d url เกิน rpc_timeout %v — "+
				"caller อาจได้ 504 ก่อนที่ระบบจะลอง url ครบ",
				g.Name, g.UpstreamTimeout, len(g.URLs), g.RPCTimeout)
		}
	}
}
