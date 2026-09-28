package store

import (
	"context"
	"sync/atomic"
)

// เหตุผลที่ถูกปฏิเสธ — แยกกันเพราะแก้คนละทาง
const (
	BlockedNotInAllowlist  = "not_in_allowlist"       // IP ไม่อยู่ใน ALLOWED_IPS
	BlockedMissingIPHeader = "missing_trusted_header" // ไม่มี CLIENT_IP_HEADER ที่ประกาศไว้
)

type BlockedInput struct {
	ClientIP string
	Path     string
	Reason   string
}

// RecordBlocked นับเพิ่มทีละครั้ง ไม่เก็บรายรายการ
func (s *Store) RecordBlocked(ctx context.Context, in BlockedInput) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO blocked_ip (client_ip, path, reason)
		VALUES (NULLIF($1,''), $2, $3)
		ON CONFLICT (client_ip, path, reason)
		DO UPDATE SET count = blocked_ip.count + 1, last_seen = now()`,
		in.ClientIP, in.Path, in.Reason)
	return err
}

// BlockedBuffer รับงานจาก HTTP handler แล้วให้ goroutine เดียวเขียนลง DB
//
// ทำแบบนี้เพราะการเขียน DB ตรงจาก handler เปิดช่องสองทาง: ถ้าเขียนแบบ synchronous
// การถูกปฏิเสธจะช้าลงตามความเร็ว DB ซึ่งกลายเป็นคันโยกให้คนยิงถล่มได้
// ส่วนถ้าแตก goroutine ใหม่ทุกครั้ง จำนวน goroutine จะไม่มีเพดาน
//
// คิวที่มีขนาดจำกัดแก้ทั้งสองอย่าง: handler ไม่เคยรอ และงานที่ล้นถูกทิ้งทันที
// การทิ้งยอมรับได้เพราะนี่คือตัวนับไว้ประเมินสถานการณ์ ไม่ใช่บัญชีที่ต้องครบทุกรายการ
// และยังมีบรรทัด log ของทุกครั้งที่ปฏิเสธอยู่แล้ว — Dropped() บอกว่าทิ้งไปเท่าไหร่
type BlockedBuffer struct {
	ch      chan BlockedInput
	record  func(context.Context, BlockedInput) error
	Logf    func(string, ...any)
	dropped atomic.Uint64
}

func NewBlockedBuffer(record func(context.Context, BlockedInput) error, size int) *BlockedBuffer {
	if size < 1 {
		size = 1
	}
	return &BlockedBuffer{
		ch:     make(chan BlockedInput, size),
		record: record,
		Logf:   func(string, ...any) {},
	}
}

// Record ไม่บล็อกเด็ดขาด — อยู่ในเส้นทางของทุก request ที่ถูกปฏิเสธ
func (b *BlockedBuffer) Record(in BlockedInput) {
	select {
	case b.ch <- in:
	default:
		b.dropped.Add(1)
	}
}

func (b *BlockedBuffer) Dropped() uint64 { return b.dropped.Load() }

// Run เขียนลง DB ทีละรายการจนกว่า ctx จะถูกยกเลิก
// เขียนไม่สำเร็จก็แค่เตือน ไม่ retry — ตัวนับที่ขาดไปไม่คุ้มกับการถือคิวไว้
func (b *BlockedBuffer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			if n := b.dropped.Load(); n > 0 {
				b.Logf("⚠️  blocked_ip: ทิ้งไป %d ครั้งเพราะคิวเต็ม", n)
			}
			return
		case in := <-b.ch:
			// ไม่ผูกกับ ctx ของ request ที่ตายไปแล้ว แต่ผูกกับอายุของ buffer
			if err := b.record(ctx, in); err != nil {
				b.Logf("⚠️  บันทึก blocked_ip ไม่สำเร็จ ip=%s: %v", in.ClientIP, err)
			}
		}
	}
}
