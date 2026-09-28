-- ตรวจผลหลังยิงโหลด — รันบน DB ของ gateway
-- k6 บอกได้แค่ว่า caller เห็นอะไร ไฟล์นี้บอกว่าข้างในเกิดอะไรขึ้นจริง
--
-- ใช้: psql "$DATABASE_URL" -v ref_prefix="'LT-%'" -f loadtest/reconcile.sql

\set ref_prefix :ref_prefix
\echo '=== 1. ผลรวม — ทุกแถวต้องลงเอยที่ success ถ้าไม่ใช่ต้องอธิบายได้ ==='
SELECT status, http_status, count(*) AS requests,
       round(avg(total_ms)) AS avg_ms, max(total_ms) AS max_ms
FROM request_logs WHERE business_ref LIKE :ref_prefix
GROUP BY 1,2 ORDER BY 3 DESC;

\echo ''
\echo '=== 2. ⚠️ เคสที่ต้องกระทบยอดด้วยมือ — caller คิดว่าล้มแต่ upstream อาจสร้างออเดอร์แล้ว ==='
SELECT count(*) AS ต้องตรวจสอบ
FROM request_logs
WHERE business_ref LIKE :ref_prefix AND http_status = 504 AND status <> 'expired';

\echo ''
\echo '=== 3. x-deadline กันไว้ได้กี่รายการ (ยิง upstream 0 ครั้ง = ปลอดภัยแน่นอน) ==='
SELECT r.status, count(DISTINCT r.trace_id) AS requests, count(a.id) AS upstream_calls
FROM request_logs r LEFT JOIN attempt_logs a USING (trace_id)
WHERE r.business_ref LIKE :ref_prefix GROUP BY 1 ORDER BY 2 DESC;

\echo ''
\echo '=== 4. latency ของ upstream — ตัวเลขที่ใช้ตั้ง worker_count ==='
SELECT count(*) AS n,
       round(avg(duration_ms)) AS avg_ms,
       percentile_cont(0.50) WITHIN GROUP (ORDER BY duration_ms)::int AS p50,
       percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)::int AS p95,
       percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms)::int AS p99,
       max(duration_ms) AS max_ms
FROM attempt_logs a JOIN request_logs r USING (trace_id)
WHERE r.business_ref LIKE :ref_prefix AND a.outcome = 'success';

\echo ''
\echo '=== 5. worker_count ที่ควรตั้ง (สูตรจาก docs/traffic-analysis.md §5) ==='
\echo '    worker = พีค rps x p95 วินาที x 3'
-- หมายเหตุ: ถ้า p95 ออกมาเป็น 0 แปลว่า upstream เร็วกว่า 1ms (เช่น /healthz ของตัวเอง)
-- ตัวเลข worker ที่ได้จะไม่มีความหมาย ต้องยิงใส่ upstream ที่มี latency ใกล้ของจริงก่อน
SELECT 1.7 AS peak_rps_จริง,
       round((percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms))::numeric / 1000, 3) AS p95_วินาที,
       greatest(1, ceil(1.7 * (percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms))::numeric / 1000 * 3))::int AS worker_ที่พอ,
       (SELECT max(worker_count) FROM message_group) AS ที่ตั้งอยู่
FROM attempt_logs a JOIN request_logs r USING (trace_id)
WHERE r.business_ref LIKE :ref_prefix AND a.outcome = 'success';

\echo ''
\echo '=== 6. failover เกิดบ่อยแค่ไหน และ url ไหนมีปัญหา ==='
SELECT a.url, a.outcome, count(*),
       round(avg(a.duration_ms)) AS avg_ms
FROM attempt_logs a JOIN request_logs r USING (trace_id)
WHERE r.business_ref LIKE :ref_prefix
GROUP BY 1,2 ORDER BY 1, 3 DESC;

\echo ''
\echo '=== 7. มี request ค้าง pending ไหม (ไม่ควรมีหลังยิงจบแล้ว) ==='
SELECT count(*) AS ค้างอยู่, min(created_at) AS เก่าสุด
FROM request_logs WHERE business_ref LIKE :ref_prefix AND status = 'pending';
