-- request ที่ถูกปฏิเสธที่ชั้น allowlist ไม่เคยถึง request_logs เลย
-- เพราะการเช็ค IP อยู่ก่อน BeginRequest ทำให้ตอบไม่ได้ว่าใครถูกบล็อกไปกี่รายการ
-- เจอปัญหานี้จริงตอน cutover 2026-09-28: ลูกค้า dual-stack ต่อผ่าน IPv6 แล้วโดน 403
-- แต่ประเมินความเสียหายไม่ได้เพราะร่องรอยมีแค่ใน log ของ container ซึ่งหายตอน restart
--
-- เก็บเป็นตัวนับ ไม่ใช่รายรายการ: 1 แถวต่อ (ip, path, reason)
-- จำนวนแถวจึงถูกจำกัดด้วยจำนวน IP ที่ไม่ซ้ำ ไม่ใช่ปริมาณ traffic
-- คนยิงมั่วล้านครั้งได้แถวเดียว ตารางไม่บวมและไม่กลายเป็นช่องขยายผล DoS
CREATE TABLE IF NOT EXISTS blocked_ip (
    client_ip  TEXT        NOT NULL,
    path       TEXT        NOT NULL,
    reason     TEXT        NOT NULL,
    count      BIGINT      NOT NULL DEFAULT 1,
    first_seen TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (client_ip, path, reason)
);

CREATE INDEX IF NOT EXISTS idx_blocked_ip_last_seen ON blocked_ip (last_seen DESC);
