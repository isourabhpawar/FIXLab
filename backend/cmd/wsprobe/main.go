// Command wsprobe subscribes to /ws/session/{token} and prints one
// "WSPROBE: <event-type>" line per received event. Used by the phase-3
// acceptance test to verify ORDER_CREATED / ORDER_UPDATED /
// EXECUTION_SENT flow over the live stream.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"nhooyr.io/websocket"
)

func main() {
	url := flag.String("url", "", "ws://host:port/ws/session/<token>")
	dur := flag.Duration("dur", 90*time.Second, "how long to listen")
	flag.Parse()
	if *url == "" {
		fmt.Fprintln(os.Stderr, "wsprobe: --url is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	c, _, err := websocket.Dial(ctx, *url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wsprobe: dial:", err)
		os.Exit(1)
	}
	defer c.Close(websocket.StatusNormalClosure, "done")
	fmt.Println("WSPROBE: CONNECTED")
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			fmt.Println("WSPROBE: CLOSED")
			return
		}
		var ev struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal(data, &ev) == nil && ev.Type != "" {
			extra := ""
			// For CONNECTION_STATUS include the status chain so tests
			// can assert ordering (CONNECTING → … → LOGON_ACCEPTED).
			if ev.Type == "CONNECTION_STATUS" && ev.Payload != nil {
				extra = fmt.Sprintf(" status=%v previous=%v", ev.Payload["status"], ev.Payload["previous"])
				if r, ok := ev.Payload["reason"]; ok && r != "" {
					extra += fmt.Sprintf(" reason=%v", r)
				}
			}
			fmt.Println("WSPROBE:", ev.Type+extra)
		}
	}
}
