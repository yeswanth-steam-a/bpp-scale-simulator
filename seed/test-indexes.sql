-- Indexes added to the load-test database on 2026-10-03 (they are NOT in the app's schema scripts).
-- Without them each OCPP message triggered full-table scans (157 billion connector rows read in one test) and the
-- stack stalled at ~500 connected chargers. With them, ~3,300 idle chargers run cleanly on a 2-vCPU database.
-- Kept in suite_scale_simulator on purpose. Re-run this on any new copy of the database (idempotent).
-- Built without CONCURRENTLY: the tables are small, and a concurrent build never finished under constant load.
SET lock_timeout = '10s';
CREATE INDEX IF NOT EXISTS saev_connectors_cp_id_btree             ON asset_db.saev_connectors (charge_point_id);
CREATE INDEX IF NOT EXISTS saev_connectors_serial_btree            ON asset_db.saev_connectors (serial_number);
CREATE INDEX IF NOT EXISTS saev_connectors_ocppid_conn_btree       ON asset_db.saev_connectors (ocpp_charge_point_id, connector_id_in_charger);
CREATE INDEX IF NOT EXISTS saev_ocpp_charge_point_cpid_btree       ON public.saev_ocpp_charge_point (charge_point_id);
CREATE INDEX IF NOT EXISTS saev_ocpp_log_cp_conn_action_btree      ON public.saev_ocpp_log (charge_point_id, connector_id, call_action);
CREATE INDEX IF NOT EXISTS saev_ocpp_log_cp_action_id_btree        ON public.saev_ocpp_log (charge_point_id, call_action, id);
CREATE INDEX IF NOT EXISTS saev_ocpp_connector_status_cp_conn_btree ON public.saev_ocpp_connector_status (charge_point_id, connector_id);
CREATE INDEX IF NOT EXISTS saev_customer_wallet_balance_cust_btree ON public.saev_customer_wallet_balance (customer_id);
