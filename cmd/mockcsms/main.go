// Command mockcsms is a tiny OCPP 1.6J server used to smoke-test the simulator without the real CSMS.
// It accepts any charger id whose number is within -max and any idTag not ending in "9".
package main

import (
	"flag"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/steam-a/bpp-load-simulator/internal/ocpp"
)

func main() {
	addr := flag.String("addr", ":9898", "listen address")
	flag.Parse()

	var conns, calls, txSeq atomic.Int64
	up := websocket.Upgrader{Subprotocols: []string{ocpp.Subprotocol}, CheckOrigin: func(*http.Request) bool { return true }}

	http.HandleFunc("/csms/", func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conns.Add(1)
		defer func() { conns.Add(-1); c.Close() }()
		for {
			_, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			f, err := ocpp.Decode(b)
			if err != nil || f.Type != ocpp.Call {
				continue
			}
			calls.Add(1)
			var res any = map[string]any{}
			switch f.Action {
			case "BootNotification":
				res = ocpp.BootNotificationRes{Status: "Accepted", Interval: 60, CurrentTime: ocpp.Now()}
			case "Heartbeat":
				res = map[string]string{"currentTime": ocpp.Now()}
			case "Authorize":
				var a ocpp.AuthorizeReq
				_ = jsonUnmarshal(f.Payload, &a)
				st := "Accepted"
				if a.IDTag != "" && a.IDTag[len(a.IDTag)-1] == '9' {
					st = "Invalid"
				}
				res = ocpp.AuthorizeRes{IDTagInfo: ocpp.IDTagInfo{Status: st}}
			case "StartTransaction":
				res = ocpp.StartTransactionRes{IDTagInfo: ocpp.IDTagInfo{Status: "Accepted"}, TransactionID: int(txSeq.Add(1))}
			}
			out, _ := ocpp.EncodeResult(f.ID, res)
			if c.WriteMessage(websocket.TextMessage, out) != nil {
				return
			}
		}
	})
	go func() {
		for range time.Tick(10 * time.Second) {
			log.Printf("mockcsms: connections=%d calls=%d transactions=%d", conns.Load(), calls.Load(), txSeq.Load())
		}
	}()
	log.Println("mock CSMS listening on", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
