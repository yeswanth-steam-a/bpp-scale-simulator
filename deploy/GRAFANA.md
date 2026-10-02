# Metrics in Grafana Cloud (free tier)

1. Create a free Grafana Cloud stack. Under *Connections > Hosted Prometheus metrics > Send metrics* note the
   push URL and numeric user, and create an access-policy token with `metrics:write`.
2. Install Grafana Alloy on the load-generator EC2 (https://grafana.com/docs/alloy/latest/set-up/install/linux/).
3. Put the three values in the Alloy service environment, copy `deploy/alloy/config.alloy` to
   `/etc/alloy/config.alloy`, restart Alloy:
   `GC_PROM_URL=... GC_PROM_USER=... GC_API_TOKEN=...`
4. In Grafana: *Dashboards > New > Import*, upload `deploy/grafana/dashboard.json`, pick the Prometheus data source.
5. Free tier retention is ~14 days: export dashboard PDFs/CSVs straight after the run.

The simulator serves Prometheus text on `:2112/metrics` (`metrics_addr` in the YAML). With 40k chargers split
across shards, give each shard host its own Alloy; the `instance` label tells them apart.
