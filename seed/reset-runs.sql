-- Clears the DATA PRODUCED BY LOAD RUNS (sessions, invoices, transactions, meter values, logs, wallet debits)
-- and resets wallets to 100000. Keeps all seeded chargers, customers, tags, stations, tariffs and levels.
-- Run between test runs: psql "$SEED_DB_URL" -v ON_ERROR_STOP=1 -f seed/reset-runs.sql
BEGIN;

DELETE FROM public.saev_invoice_details
 WHERE transaction_type = 'SESSION_INVOICE'
   AND reference_id IN (SELECT id FROM public.saev_charging_session WHERE user_rfid LIKE 'LTTAG%');
DELETE FROM public.saev_charging_session_log
 WHERE session_id IN (SELECT id FROM public.saev_charging_session WHERE user_rfid LIKE 'LTTAG%');
DELETE FROM public.saev_wallet_transaction
 WHERE ocpp_transaction_id::text IN (SELECT transaction_id::text FROM public.saev_ocpp_transaction WHERE charge_point_id LIKE 'LT-%');
DELETE FROM public.saev_charging_session WHERE user_rfid LIKE 'LTTAG%';

DELETE FROM public.saev_ocpp_meter_value WHERE charge_point_id LIKE 'LT-%';
DELETE FROM public.saev_ocpp_log         WHERE charge_point_id LIKE 'LT-%';
DELETE FROM public.saev_ocpp_transaction WHERE charge_point_id LIKE 'LT-%';

UPDATE public.saev_customer_wallet_balance SET wallet_balance = 100000, updated_at = now()
 WHERE customer_id IN (SELECT id FROM user_db.suite_users WHERE email LIKE 'loadtest+%@steam-a.com');

UPDATE asset_db.saev_charge_point SET total_sessions = 0, total_energy_consumed = 0 WHERE ocpp_charge_point_id LIKE 'LT-%';
UPDATE asset_db.station SET total_sessions = 0, energy_delivered = 0;
-- Runs stopped mid-session leave connectors Charging/Preparing/Finishing/Unavailable in the CMS.
UPDATE asset_db.saev_connectors SET current_status = 'Available'
 WHERE ocpp_charge_point_id LIKE 'LT-%' AND current_status IS DISTINCT FROM 'Available';

COMMIT;
