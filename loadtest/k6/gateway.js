// ยิงโหลดใส่ MQ Gateway v2
//
// ระดับโหลดทั้งหมดอิงจาก docs/traffic-analysis.md (ข้อมูลจริง 7 วัน 2026-09-17..24)
//   เฉลี่ย  10.6 รายการ/นาที = 0.18/วิ
//   p99     35 รายการ/นาที   = 0.58/วิ
//   พีคจริง 103 รายการ/นาที  = 1.7/วิ   ← TARGET
//
// ตัวเลขนี้นับเฉพาะรายการที่ไปถึง backend สำเร็จ รายการที่ตายตอน TLS handshake
// ไม่ถูกนับเลย ของจริงจึงสูงกว่านี้ — เป็นเหตุผลที่ stress ไต่ไปไกลกว่าพีคมาก
import http from 'k6/http'
import { check } from 'k6'
import { Counter, Trend } from 'k6/metrics'

const BASE = __ENV.BASE || 'https://deposit-service-mq-v2.ebwved.easypanel.host'
const GROUP = __ENV.GROUP || 'loadtest'
const TOKEN = __ENV.TOKEN || ''
const TARGET = Number(__ENV.TARGET || 2) // rps — พีคจริง 1.7 ปัดขึ้นเป็น 2

const nonOK = new Counter('gateway_non_200')
const gwLatency = new Trend('gateway_latency_ms', true)

const SCENARIOS = {
  // เส้นทางยังถูกไหม — รันก่อนทุกครั้ง
  smoke: { executor: 'constant-arrival-rate', rate: TARGET, timeUnit: '1s', duration: '30s',
           preAllocatedVUs: 10, maxVUs: 50 },

  // ที่พีคจริง นิ่งไหมเมื่อรันยาว
  load: { executor: 'constant-arrival-rate', rate: TARGET, timeUnit: '1s', duration: '10m',
          preAllocatedVUs: 20, maxVUs: 100 },

  // เพดานอยู่ตรงไหน และพังแบบไหน — ไต่ถึง 100 เท่าของพีคจริง
  stress: { executor: 'ramping-arrival-rate', startRate: TARGET, timeUnit: '1s',
            preAllocatedVUs: 50, maxVUs: 1000,
            stages: [
              { target: TARGET * 5, duration: '1m' },
              { target: TARGET * 25, duration: '2m' },
              { target: TARGET * 100, duration: '3m' },
              { target: TARGET * 100, duration: '2m' },
              { target: TARGET, duration: '1m' },
            ] },

  // จำลองพีคฉับพลันแบบ 2026-09-19 20:52 (103 รายการในนาทีเดียว)
  spike: { executor: 'ramping-arrival-rate', startRate: TARGET, timeUnit: '1s',
           preAllocatedVUs: 50, maxVUs: 500,
           stages: [
             { target: TARGET, duration: '30s' },
             { target: TARGET * 12, duration: '10s' },
             { target: TARGET * 12, duration: '1m' },
             { target: TARGET, duration: '30s' },
             { target: TARGET, duration: '1m' },
           ] },

  // รั่วไหม คิวค้างสะสมไหม
  soak: { executor: 'constant-arrival-rate', rate: Math.max(1, Math.round(TARGET / 2)),
          timeUnit: '1s', duration: '2h', preAllocatedVUs: 20, maxVUs: 100 },
}

const pick = __ENV.SCENARIO || 'smoke'
export const options = {
  scenarios: { [pick]: SCENARIOS[pick] },
  thresholds: {
    // พีคจริงคือ 1.7/วิ — ถ้า p95 เกิน 2 วินาทีที่โหลดระดับนั้นแปลว่ามีอะไรผิด
    'http_req_duration{expected_response:true}': ['p(95)<2000'],
    gateway_non_200: ['count<1'],
  },
}

export default function () {
  const ref = `LT-${pick}-${__VU}-${__ITER}`
  const res = http.post(`${BASE}/${GROUP}`,
    JSON.stringify({ customer_order_id: ref, amount: 100, mid: 'loadtest' }), {
      headers: {
        'Content-Type': 'application/json',
        ...(TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {}),
      },
      timeout: '120s',
      tags: { name: 'POST /'+GROUP },
    })

  gwLatency.add(res.timings.duration)
  const ok = check(res, {
    'status 200': (r) => r.status === 200,
    'มี x-trace-id': (r) => !!r.headers['X-Trace-Id'],
  })
  if (!ok) {
    nonOK.add(1)
    if (nonOK.value <= 5) console.error(`${ref} → ${res.status} ${String(res.body).slice(0, 120)}`)
  }
}
