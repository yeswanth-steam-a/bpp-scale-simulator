-- Give the AC 7.4 kW load-test chargers a second connector (77,500 -> 100,000 connectors on 50,000 chargers).
-- Usage: psql "$SEED_DB_URL" -v ON_ERROR_STOP=1 -v first=1 -v last=22500 -f seed/add-second-connector.sql
-- Acts on LT-<first>..LT-<last> that still have exactly one connector; safe to re-run (already-done chargers are skipped).
-- Connector 2 is a copy of connector 1 with its own id (charge_connector_id = id, like the existing rows).
BEGIN;

CREATE TEMP TABLE _one ON COMMIT DROP AS
SELECT c.*
FROM asset_db.saev_connectors c
WHERE c.ocpp_charge_point_id BETWEEN 'LT-' || lpad(:'first', 6, '0') AND 'LT-' || lpad(:'last', 6, '0')
  AND c.ocpp_charge_point_id ~ '^LT-[0-9]{6}$'
  AND c.connector_id_in_charger = 1
  AND NOT EXISTS (SELECT 1 FROM asset_db.saev_connectors c2
                  WHERE c2.ocpp_charge_point_id = c.ocpp_charge_point_id AND c2.connector_id_in_charger = 2);

WITH n AS (SELECT o.*, nextval('asset_db.saev_connectors_id_seq') AS nid FROM _one o)
INSERT INTO asset_db.saev_connectors
SELECT (jsonb_populate_record(NULL::asset_db.saev_connectors,
         to_jsonb(n) - 'nid' || jsonb_build_object('id', n.nid, 'charge_connector_id', n.nid,
                                                    'connector_id_in_charger', 2, 'current_status', 'Available',
                                                    'last_received_message_time', NULL))).*
FROM n;

UPDATE asset_db.saev_charge_point p SET no_of_connectors = 2
WHERE p.serial_number IN (SELECT ocpp_charge_point_id FROM _one);

UPDATE public.saev_ocpp_charge_point o SET connectors = 2
WHERE o.charge_point_id IN (SELECT ocpp_charge_point_id FROM _one);

SELECT count(*) AS chargers_given_a_second_connector FROM _one;
COMMIT;
