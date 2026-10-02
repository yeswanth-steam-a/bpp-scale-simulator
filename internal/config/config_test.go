package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIDsFileFiltersAndDedupes(t *testing.T) {
	dir := t.TempDir()
	ids := filepath.Join(dir, "ids.txt")
	if err := os.WriteFile(ids, []byte("T1\nT1\n\nCP_102\nhas space\nbad!id\nALB-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yml := filepath.Join(dir, "c.yaml")
	body := "target: ws://x/csms/\nids_file: " + ids + "\nduration: 1h\nsessions: {total: 1, curve: [1]}\n" +
		"profiles: [{name: a, share: 1, connectors: 1, max_power_w: 7400, meter_interval: 60s, session_mean: 10m, session_weight: 1}]\n"
	if err := os.WriteFile(yml, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(yml)
	if err != nil {
		t.Fatal(err)
	}
	if c.Chargers != 3 || c.ChargePointID(0) != "T1" || c.ChargePointID(2) != "ALB-1" || !c.UsesIDList() {
		t.Fatalf("unexpected result: chargers=%d ids=%v", c.Chargers, c.ids)
	}
}

func TestTagsFileSkipsVirtualIDs(t *testing.T) {
	dir := t.TempDir()
	tags := filepath.Join(dir, "tags.txt")
	if err := os.WriteFile(tags, []byte("RFID1\nVID:abc\nvid:def\nRFID1\nRFID2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yml := filepath.Join(dir, "c.yaml")
	body := "target: ws://x/csms/\nchargers: 1\nduration: 1h\nsessions: {total: 1, curve: [1]}\nidtags: {file: " + tags + "}\n" +
		"profiles: [{name: a, share: 1, connectors: 1, max_power_w: 7400, meter_interval: 60s, session_mean: 10m, session_weight: 1}]\n"
	if err := os.WriteFile(yml, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(yml)
	if err != nil {
		t.Fatal(err)
	}
	if c.IDTags.Pool != 2 || c.IDTag(0) != "RFID1" || c.IDTag(1) != "RFID2" {
		t.Fatalf("unexpected tags: %v pool=%d", c.IDTags.List, c.IDTags.Pool)
	}
}

func TestIDsFileConnectorCounts(t *testing.T) {
	dir := t.TempDir()
	ids := filepath.Join(dir, "ids.txt")
	if err := os.WriteFile(ids, []byte("A1,2\nB2, 5\nC3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yml := filepath.Join(dir, "c.yaml")
	body := "target: ws://x/csms/\nids_file: " + ids + "\nduration: 1h\nsessions: {total: 1, curve: [1]}\n" +
		"profiles: [{name: a, share: 1, connectors: 1, max_power_w: 7400, meter_interval: 60s, session_mean: 10m, session_weight: 1}]\n"
	if err := os.WriteFile(yml, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(yml)
	if err != nil {
		t.Fatal(err)
	}
	if c.ChargePointID(1) != "B2" || c.ConnectorsFor(0) != 2 || c.ConnectorsFor(1) != 5 || c.ConnectorsFor(2) != 1 {
		t.Fatalf("ids=%v conns=%v", c.ids, c.idConns)
	}
}
