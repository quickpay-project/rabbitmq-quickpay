package amqpx

import (
	"context"
	"time"
)

// Retry เรียก fn ซ้ำจนสำเร็จ โดยหน่วงแบบทวีคูณและหยุดทันทีที่ ctx ถูกยกเลิก
// แยกออกมาเป็นฟังก์ชันบริสุทธิ์เพื่อให้ทดสอบได้โดยไม่ต้องรอเวลาจริง
func Retry(ctx context.Context, initial, max time.Duration,
	sleep func(time.Duration), fn func() error) error {

	backoff := initial
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn()
		if err == nil {
			return nil
		}
		sleep(backoff)
		if err := ctx.Err(); err != nil {
			return err
		}
		if backoff < max {
			backoff *= 2
			if backoff > max {
				backoff = max
			}
		}
	}
}
