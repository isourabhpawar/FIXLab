// Command fixclient is the Phase 1 acceptance-test FIX client: a real,
// external QuickFIX/Go initiator that connects to a FixLab acceptor
// session, completes logon, exchanges heartbeats, answers a TestRequest,
// and logs out cleanly.
//
// It exits 0 only when the full lifecycle succeeds, printing markers the
// test harness greps for:
//
//	FIXCLIENT: LOGON_OK
//	FIXCLIENT: HEARTBEAT_RX
//	FIXCLIENT: TESTREQ_OK
//	FIXCLIENT: LOGOUT_OK
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"
)

const (
	tagMsgType   = quickfix.Tag(35)
	tagTestReqID = quickfix.Tag(112)
	tagText      = quickfix.Tag(58)
	testReqID    = "FIXLAB-TR-1"
)

type app struct {
	sessionID  atomic.Value // quickfix.SessionID
	loggedOn   chan struct{}
	loggedOut  chan struct{}
	heartbeats atomic.Int64
	testReqOK  atomic.Bool
}

func (a *app) OnCreate(id quickfix.SessionID) {
	a.sessionID.Store(id)
	fmt.Println("FIXCLIENT: session created", id)
}
func (a *app) OnLogon(id quickfix.SessionID) {
	fmt.Println("FIXCLIENT: LOGON_OK")
	// The initiator may reconnect and log on again; never close twice.
	select {
	case <-a.loggedOn:
	default:
		close(a.loggedOn)
	}
}
func (a *app) OnLogout(id quickfix.SessionID) {
	fmt.Println("FIXCLIENT: LOGOUT_OK")
	select {
	case <-a.loggedOut:
	default:
		close(a.loggedOut)
	}
}
func (a *app) ToAdmin(msg *quickfix.Message, _ quickfix.SessionID)     {}
func (a *app) ToApp(msg *quickfix.Message, _ quickfix.SessionID) error { return nil }
func (a *app) FromApp(msg *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	mt, _ := msg.Header.GetString(tagMsgType)
	if mt == "8" || mt == "9" {
		raw := strings.ReplaceAll(msg.String(), "\x01", "|")
		fmt.Printf("FIXCLIENT: ER_RX %s\n", raw)
		var sb strings.Builder
		for _, t := range []quickfix.Tag{11, 17, 37, 41, 38, 44, 14, 151, 32, 31, 6, 150, 39, 103, 102, 434, 58} {
			if v, err := msg.Body.GetString(t); err == nil {
				fmt.Fprintf(&sb, "%d=%s ", int(t), v)
			}
		}
		fmt.Printf("FIXCLIENT: ER_FIELDS %s\n", strings.TrimSpace(sb.String()))
	}
	return nil
}
func (a *app) FromAdmin(msg *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	mt, _ := msg.Header.GetString(tagMsgType)
	switch mt {
	case "0":
		n := a.heartbeats.Add(1)
		tr, _ := msg.Body.GetString(tagTestReqID) // 112 is a body field on Heartbeat
		fmt.Printf("FIXCLIENT: HEARTBEAT_RX #%d testReqID=%q\n", n, tr)
		if tr == testReqID {
			a.testReqOK.Store(true)
			fmt.Println("FIXCLIENT: TESTREQ_OK")
		}
	case "1":
		fmt.Println("FIXCLIENT: test request received (quickfix auto-responds)")
	case "5":
		fmt.Println("FIXCLIENT: logout received")
	}
	return nil
}

func sendAdmin(sessionID quickfix.SessionID, msgType string, header, body map[quickfix.Tag]string) error {
	msg := quickfix.NewMessage()
	msg.Header.SetString(tagMsgType, msgType)
	for t, v := range header {
		msg.Header.SetString(t, v)
	}
	for t, v := range body {
		msg.Body.SetString(t, v)
	}
	return quickfix.SendToTarget(msg, sessionID)
}

// sendRaw builds one application message with an arbitrary body field
// set (phase 2.4 e2e: inject custom tags like 9001=B). Outbound
// messages are not dictionary-validated by QuickFIX/Go, so any tag
// passes; the receiving side decides what to do with it.
func sendRaw(sessionID quickfix.SessionID, msgType string, kvs []string) error {
	msg := quickfix.NewMessage()
	msg.Header.SetString(tagMsgType, msgType)
	for _, kv := range kvs {
		tag, val, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("bad tag=value %q", kv)
		}
		n, err := strconv.Atoi(strings.TrimSpace(tag))
		if err != nil || n <= 0 {
			return fmt.Errorf("bad tag %q", tag)
		}
		msg.Body.SetString(quickfix.Tag(n), val)
	}
	return quickfix.SendToTarget(msg, sessionID)
}

const (
	tagClOrdID    = quickfix.Tag(11)
	tagOrigClOrd  = quickfix.Tag(41)
	tagSymbol     = quickfix.Tag(55)
	tagSide       = quickfix.Tag(54)
	tagOrderQty   = quickfix.Tag(38)
	tagOrdType    = quickfix.Tag(40)
	tagPrice      = quickfix.Tag(44)
	tagTransactTm = quickfix.Tag(60)
)

func transactTime() string {
	return time.Now().UTC().Format("20060102-15:04:05.000")
}

func sendNOS(sessionID quickfix.SessionID, clOrdID, symbol, side, qty, price, ordType string) error {
	msg := quickfix.NewMessage()
	msg.Header.SetString(tagMsgType, "D")
	msg.Body.SetString(tagClOrdID, clOrdID)
	msg.Body.SetString(tagSymbol, symbol)
	msg.Body.SetString(tagSide, side)
	msg.Body.SetString(tagOrderQty, qty)
	msg.Body.SetString(tagOrdType, ordType)
	if price != "" {
		msg.Body.SetString(tagPrice, price)
	}
	msg.Body.SetString(tagTransactTm, transactTime())
	return quickfix.SendToTarget(msg, sessionID)
}

func sendCancel(sessionID quickfix.SessionID, clOrdID, origClOrdID string) error {
	msg := quickfix.NewMessage()
	msg.Header.SetString(tagMsgType, "F")
	msg.Body.SetString(tagClOrdID, clOrdID)
	msg.Body.SetString(tagOrigClOrd, origClOrdID)
	msg.Body.SetString(tagSide, "1")
	msg.Body.SetString(tagTransactTm, transactTime())
	return quickfix.SendToTarget(msg, sessionID)
}

func sendReplace(sessionID quickfix.SessionID, clOrdID, origClOrdID, qty, price string) error {
	msg := quickfix.NewMessage()
	msg.Header.SetString(tagMsgType, "G")
	msg.Body.SetString(tagClOrdID, clOrdID)
	msg.Body.SetString(tagOrigClOrd, origClOrdID)
	msg.Body.SetString(tagSide, "1")
	msg.Body.SetString(tagOrderQty, qty)
	msg.Body.SetString(tagOrdType, "2")
	if price != "" {
		msg.Body.SetString(tagPrice, price)
	}
	msg.Body.SetString(tagTransactTm, transactTime())
	return quickfix.SendToTarget(msg, sessionID)
}

// runScript reads newline-separated commands from stdin after logon:
//
//	NOS <clOrdID> <symbol> <side> <qty> <price> <ordType>
//	CANCEL <clOrdID> <origClOrdID>
//	REPLACE <clOrdID> <origClOrdID> <qty> <price>
//	RAW <msgType> tag=value [tag=value ...]   (phase 2.4: arbitrary body fields, e.g. custom tags)
//	LOGOUT
//
// It returns when LOGOUT is processed or stdin closes.
func runScript(sessionID quickfix.SessionID) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		var err error
		switch strings.ToUpper(parts[0]) {
		case "NOS":
			if len(parts) != 7 {
				fmt.Println("FIXCLIENT: script: NOS needs 6 args")
				continue
			}
			err = sendNOS(sessionID, parts[1], parts[2], parts[3], parts[4], parts[5], parts[6])
		case "CANCEL":
			if len(parts) != 3 {
				fmt.Println("FIXCLIENT: script: CANCEL needs 2 args")
				continue
			}
			err = sendCancel(sessionID, parts[1], parts[2])
		case "REPLACE":
			if len(parts) != 5 {
				fmt.Println("FIXCLIENT: script: REPLACE needs 4 args")
				continue
			}
			err = sendReplace(sessionID, parts[1], parts[2], parts[3], parts[4])
		case "RAW":
			if len(parts) < 3 {
				fmt.Println("FIXCLIENT: script: RAW needs msgType and at least one tag=value")
				continue
			}
			err = sendRaw(sessionID, parts[1], parts[2:])
		case "LOGOUT":
			fmt.Println("FIXCLIENT: script: LOGOUT requested")
			return
		default:
			fmt.Println("FIXCLIENT: script: unknown command", parts[0])
			continue
		}
		if err != nil {
			fmt.Println("FIXCLIENT: script: send failed:", err)
			continue
		}
		fmt.Println("FIXCLIENT: script: sent", parts[0], parts[1])
	}
}

func main() {
	host := flag.String("host", "127.0.0.1", "FixLab acceptor host")
	port := flag.Int("port", 0, "FixLab acceptor port (required)")
	sender := flag.String("sender", "FIXCLIENT", "local SenderCompID")
	target := flag.String("target", "FIXLAB", "remote TargetCompID")
	heartBt := flag.Int("heartbt", 30, "HeartBtInt seconds")
	stay := flag.Duration("stay", 75*time.Second, "how long to remain logged on")
	dict := flag.String("dict", "backend/specs/FIX44.xml", "path to FIX44 data dictionary")
	script := flag.Bool("script", false, "read NOS/CANCEL/REPLACE/LOGOUT commands from stdin after logon")
	useTLS := flag.Bool("tls", false, "connect over TLS (test-only: skips server certificate verification)")
	flag.Parse()

	if *port == 0 {
		fmt.Fprintln(os.Stderr, "fixclient: --port is required")
		os.Exit(2)
	}

	// --tls uses QuickFIX/Go's native TLS client (stdlib, TLS 1.2+);
	// verification is skipped because test servers use self-signed
	// certificates. This is a TEST client only.
	tlsSettings := ""
	if *useTLS {
		tlsSettings = "SocketUseSSL=Y\nSocketInsecureSkipVerify=Y\n"
	}

	settingsText := fmt.Sprintf(`[DEFAULT]
ConnectionType=initiator
BeginString=FIX.4.4
SenderCompID=%s
TargetCompID=%s
SocketConnectHost=%s
SocketConnectPort=%d
HeartBtInt=%d
DataDictionary=%s
%sReconnectInterval=5
ResetOnLogon=Y
[SESSION]
BeginString=FIX.4.4
SenderCompID=%s
TargetCompID=%s
`, *sender, *target, *host, *port, *heartBt, *dict, tlsSettings, *sender, *target)

	settings, err := quickfix.ParseSettings(strings.NewReader(settingsText))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixclient: settings:", err)
		os.Exit(2)
	}
	a := &app{loggedOn: make(chan struct{}), loggedOut: make(chan struct{})}
	initiator, err := quickfix.NewInitiator(a, quickfix.NewMemoryStoreFactory(), settings, quickfix.NewNullLogFactory())
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixclient: initiator:", err)
		os.Exit(2)
	}
	if err := initiator.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "fixclient: start:", err)
		os.Exit(2)
	}
	defer initiator.Stop()

	fail := func(msg string) {
		fmt.Fprintln(os.Stderr, "fixclient: FAIL:", msg)
		os.Exit(1)
	}

	// 1. Logon.
	select {
	case <-a.loggedOn:
	case <-time.After(20 * time.Second):
		fail("logon not completed within 20s")
	}
	sid := a.sessionID.Load().(quickfix.SessionID)

	// 2. TestRequest round-trip. Per FIX, TestReqID (112) is a body field
	// of the TestRequest, not a header field.
	if err := sendAdmin(sid, "1", nil, map[quickfix.Tag]string{tagTestReqID: testReqID}); err != nil {
		fail(fmt.Sprintf("send TestRequest: %v", err))
	}
	fmt.Println("FIXCLIENT: test request sent")
	deadline := time.After(20 * time.Second)
	for !a.testReqOK.Load() {
		select {
		case <-deadline:
			fail("no heartbeat answering our TestRequest within 20s")
		case <-time.After(200 * time.Millisecond):
		}
	}

	// 3. Stay connected to exchange scheduled heartbeats, or run the
	// scripted order flow (phase 3 acceptance tests).
	if *script {
		fmt.Println("FIXCLIENT: script mode — reading commands from stdin")
		runScript(sid)
	} else {
		fmt.Println("FIXCLIENT: staying connected for", stay)
		time.Sleep(*stay)
	}
	if n := a.heartbeats.Load(); n < 1 {
		fail(fmt.Sprintf("expected >=1 heartbeat, got %d", n))
	}

	// 4. Clean logout.
	if err := sendAdmin(sid, "5", nil, map[quickfix.Tag]string{tagText: "fixclient done"}); err != nil {
		fail(fmt.Sprintf("send Logout: %v", err))
	}
	select {
	case <-a.loggedOut:
	case <-time.After(15 * time.Second):
		fail("logout not acknowledged within 15s")
	}

	fmt.Println("FIXCLIENT: lifecycle complete — all checks passed")
}
