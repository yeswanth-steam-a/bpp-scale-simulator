// Command probe connects one or more chargers to a CSMS and runs one short session on each, all at the
// same time, printing a per-charger result. It exists to check real-server behaviour (e.g. whether the
// same idTag can be used by several chargers at once). Without -tag it only boots and heartbeats.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/steam-a/bpp-load-simulator/internal/ocpp"
)

type result struct {
	id, boot, auth, start, stop string
	txID                        int
	authMs, startMs, stopMs     int64
	err                         string
}

func main() {
	target := flag.String("target", "ws://35.154.56.240:9890/csms/", "CSMS base url ending in /csms/")
	ids := flag.String("id", "T1", "comma-separated charge point ids (one session each, run concurrently)")
	tag := flag.String("tag", "", "idTag; if set, runs one short session per charger")
	connector := flag.Int("connector", 1, "connector id for the session")
	hold := flag.Duration("hold", 10*time.Second, "how long each session charges before stopping")
	verbose := flag.Bool("v", false, "print raw frames")
	stopTx := flag.Int("stop-tx", 0, "instead of a session, send StopTransaction for this open transaction id (needs -tag and -meter-stop)")
	meterStop := flag.Int("meter-stop", 0, "meter reading (Wh) to report with -stop-tx")
	flag.Parse()

	list := strings.Split(*ids, ",")
	results := make([]*result, len(list))
	var wg sync.WaitGroup
	for i, id := range list {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			results[i] = run(*target, strings.TrimSpace(id), *tag, *connector, *hold, *verbose, *stopTx, *meterStop)
		}(i, id)
	}
	wg.Wait()

	fmt.Printf("\n%-10s %-9s %-9s %-24s %-9s\n", "charger", "boot", "authorize", "start", "stop")
	for _, r := range results {
		start := r.start
		if r.txID > 0 {
			start = fmt.Sprintf("%s tx=%d", r.start, r.txID)
		}
		fmt.Printf("%-10s %-9s %-9s %-24s %-9s %s\n", r.id, r.boot,
			fmt.Sprintf("%s %dms", r.auth, r.authMs), fmt.Sprintf("%s %dms", start, r.startMs), fmt.Sprintf("%s %dms", r.stop, r.stopMs), r.err)
	}
}

func run(target, id, tag string, connector int, hold time.Duration, verbose bool, stopTx, meterStop int) *result {
	r := &result{id: id, boot: "-", auth: "-", start: "-", stop: "-"}
	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Subprotocols: []string{ocpp.Subprotocol}}
	c, resp, err := d.Dial(target+id, http.Header{})
	if err != nil {
		r.err = "connect failed"
		if resp != nil {
			r.err = fmt.Sprintf("handshake HTTP %d", resp.StatusCode)
		}
		return r
	}
	defer c.Close()

	n := 0
	call := func(action string, payload any) (*ocpp.Frame, time.Duration) {
		n++
		b, _ := ocpp.EncodeCall(fmt.Sprint(n), action, payload)
		start := time.Now()
		if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
			log.Printf("%s write: %v", id, err)
			return nil, 0
		}
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		for {
			_, raw, err := c.ReadMessage()
			if err != nil {
				r.err = "read: " + err.Error()
				return nil, time.Since(start)
			}
			if verbose {
				fmt.Printf("[%s] %s -> %s\n[%s] %s <- %s\n", id, action, b, id, action, raw)
			}
			f, err := ocpp.Decode(raw)
			if err == nil && f.Type != ocpp.Call {
				return f, time.Since(start)
			}
		}
	}
	status := func(conn int, st string) {
		call("StatusNotification", ocpp.StatusNotificationReq{ConnectorID: conn, ErrorCode: "NoError", Status: st, Timestamp: ocpp.Now()})
	}

	f, _ := call("BootNotification", ocpp.BootNotificationReq{ChargePointVendor: "LoadSim", ChargePointModel: "Probe", ChargePointSerialNumber: id, FirmwareVersion: "probe"})
	if f == nil {
		return r
	}
	var boot ocpp.BootNotificationRes
	_ = jsonUnmarshal(f.Payload, &boot)
	r.boot = boot.Status
	status(0, "Available")
	if stopTx > 0 {
		f, dt := call("StopTransaction", ocpp.StopTransactionReq{IDTag: tag, MeterStop: meterStop, Timestamp: ocpp.Now(), TransactionID: stopTx, Reason: "Local"})
		r.stopMs = dt.Milliseconds()
		r.stop = "ok"
		if f == nil {
			r.stop = "no reply"
		}
		status(connector, "Available")
		return r
	}
	if tag == "" {
		return r
	}

	status(connector, "Preparing")
	f, dt := call("Authorize", ocpp.AuthorizeReq{IDTag: tag})
	r.authMs = dt.Milliseconds()
	if f == nil {
		return r
	}
	var au ocpp.AuthorizeRes
	_ = jsonUnmarshal(f.Payload, &au)
	r.auth = au.IDTagInfo.Status
	if au.IDTagInfo.Status != "Accepted" {
		status(connector, "Available")
		return r
	}

	f, dt = call("StartTransaction", ocpp.StartTransactionReq{ConnectorID: connector, IDTag: tag, MeterStart: 1000, Timestamp: ocpp.Now()})
	r.startMs = dt.Milliseconds()
	if f == nil {
		return r
	}
	var st ocpp.StartTransactionRes
	_ = jsonUnmarshal(f.Payload, &st)
	r.start, r.txID = st.IDTagInfo.Status, st.TransactionID
	if st.TransactionID <= 0 || st.IDTagInfo.Status != "Accepted" {
		status(connector, "Available")
		return r
	}
	status(connector, "Charging")
	tx := st.TransactionID
	wh := 1000
	for t := 5 * time.Second; t <= hold; t += 5 * time.Second {
		time.Sleep(5 * time.Second)
		wh += 100
		call("MeterValues", ocpp.MeterValuesReq{ConnectorID: connector, TransactionID: &tx, MeterValue: []ocpp.MeterValue{{
			Timestamp: ocpp.Now(), SampledValue: []ocpp.SampledValue{
				{Value: fmt.Sprint(wh), Context: "Sample.Periodic", Format: "Raw", Measurand: "Energy.Active.Import.Register", Location: "Outlet", Unit: "Wh"},
				{Value: "7000", Context: "Sample.Periodic", Format: "Raw", Measurand: "Power.Active.Import", Location: "Outlet", Unit: "W"},
			}}}})
	}
	status(connector, "Finishing")
	f, dt = call("StopTransaction", ocpp.StopTransactionReq{IDTag: tag, MeterStop: wh, Timestamp: ocpp.Now(), TransactionID: tx, Reason: "Local"})
	r.stopMs = dt.Milliseconds()
	r.stop = "ok"
	if f == nil {
		r.stop = "no reply"
	}
	status(connector, "Available")
	return r
}
