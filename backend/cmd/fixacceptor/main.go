// Command fixacceptor is the Phase 5 acceptance-test counterparty: a
// real, external QuickFIX/Go ACCEPTOR that the FixLab initiator dials.
//
// Scripted behavior (the "remote broker"):
//   - 35=D NewOrderSingle → logs ACCEPTOR: NOS_RX, sends the New ack
//     (150=0/39=0); unless the symbol starts with "HOLD", immediately
//     follows with a full-fill ER (150=F/39=2). HOLD* symbols stay
//     working so the harness can test cancel/replace against them.
//   - 35=F OrderCancelRequest → logs ACCEPTOR: CANCEL_RX, sends the
//     cancel ER (150=4/39=4).
//   - 35=G OrderCancelReplaceRequest → logs ACCEPTOR: REPLACE_RX, sends
//     the replace ER (150=5/39=5) echoing the requested qty/price.
//
// It exits non-zero only on startup failure; the e2e script kills it
// when done.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"

	"fixlab.dev/fixlab/backend/internal/security"
)

type app struct {
	loggedOn  chan struct{}
	execSeq   atomic.Int64
	orderSeq  atomic.Int64
	sender    string
	target    string
}

func (a *app) OnCreate(_ quickfix.SessionID) {}
func (a *app) OnLogon(_ quickfix.SessionID) {
	fmt.Println("ACCEPTOR: LOGON_OK")
	select {
	case <-a.loggedOn:
	default:
		close(a.loggedOn)
	}
}
func (a *app) OnLogout(_ quickfix.SessionID) { fmt.Println("ACCEPTOR: LOGOUT") }
func (a *app) ToAdmin(_ *quickfix.Message, _ quickfix.SessionID)     {}
func (a *app) ToApp(_ *quickfix.Message, _ quickfix.SessionID) error { return nil }
func (a *app) FromAdmin(msg *quickfix.Message, _ quickfix.SessionID) quickfix.MessageRejectError {
	if mt, _ := msg.Header.GetString(quickfix.Tag(35)); mt == "1" {
		fmt.Println("ACCEPTOR: test request received (quickfix auto-responds)")
	}
	return nil
}

func get(msg *quickfix.Message, tag quickfix.Tag) string {
	v, _ := msg.Body.GetString(tag)
	return v
}

// sendER emits the ExecutionReport with fields in FIX44 dictionary
// order and only dictionary-defined fields: the FixLab initiator (like
// any QuickFIX/Go counterparty) validates inbound messages strictly
// (CheckFieldsOutOfOrder + no unknown fields), so the scripted
// counterparty must speak well-formed FIX.
func (a *app) sendER(sid quickfix.SessionID, clOrdID, origClOrdID, side string, execType, ordStatus, price, lastQty, lastPx, cumQty, leavesQty, avgPx string) {
	execID := fmt.Sprintf("EX%06d", a.execSeq.Add(1))
	orderID := fmt.Sprintf("RMT%06d", a.orderSeq.Add(1))
	msg := quickfix.NewMessage()
	msg.Header.SetString(quickfix.Tag(35), "8")
	// NOTE: &msg.Body, not a copy — copying the FieldMap struct loses
	// the tag ordering and silently empties the serialized body.
	b := &msg.Body
	// Dictionary order for FIX44 ExecutionReport.
	b.SetString(quickfix.Tag(37), orderID)
	b.SetString(quickfix.Tag(11), clOrdID)
	if origClOrdID != "" {
		b.SetString(quickfix.Tag(41), origClOrdID)
	}
	b.SetString(quickfix.Tag(17), execID)
	b.SetString(quickfix.Tag(150), execType)
	b.SetString(quickfix.Tag(39), ordStatus)
	if side != "" {
		b.SetString(quickfix.Tag(54), side)
	}
	if price != "" {
		b.SetString(quickfix.Tag(44), price)
	}
	if lastQty != "" {
		b.SetString(quickfix.Tag(32), lastQty)
	}
	if lastPx != "" {
		b.SetString(quickfix.Tag(31), lastPx)
	}
	if leavesQty != "" {
		b.SetString(quickfix.Tag(151), leavesQty)
	}
	if cumQty != "" {
		b.SetString(quickfix.Tag(14), cumQty)
	}
	px := avgPx
	if px == "" {
		px = price
	}
	if px != "" {
		b.SetString(quickfix.Tag(6), px)
	}
	b.SetString(quickfix.Tag(60), time.Now().UTC().Format("20060102-15:04:05.000"))
	if err := quickfix.SendToTarget(msg, sid); err != nil {
		fmt.Println("ACCEPTOR: send ER failed:", err)
		return
	}
	raw := strings.ReplaceAll(msg.String(), "\x01", "|")
	fmt.Printf("ACCEPTOR: ER_TX %s\n", raw)
}

func (a *app) FromApp(msg *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	mt, _ := msg.Header.GetString(quickfix.Tag(35))
	raw := strings.ReplaceAll(msg.String(), "\x01", "|")
	switch mt {
	case "D":
		clOrdID, symbol, side, qty, price, ordType :=
			get(msg, 11), get(msg, 55), get(msg, 54), get(msg, 38), get(msg, 44), get(msg, 40)
		fmt.Printf("ACCEPTOR: NOS_RX 11=%s 55=%s 54=%s 38=%s 40=%s 44=%s\n", clOrdID, symbol, side, qty, ordType, price)
		fmt.Printf("ACCEPTOR: NOS_RAW %s\n", raw)
		// New ack first (keeps the initiator's order state consistent).
		a.sendER(sid, clOrdID, "", side, "0", "0", price, "", "", "0", qty, "")
		if strings.HasPrefix(symbol, "HOLD") {
			fmt.Printf("ACCEPTOR: HOLD symbol %s — leaving order working\n", symbol)
			return nil
		}
		// Immediate full fill.
		a.sendER(sid, clOrdID, "", side, "F", "2", price, qty, price, qty, "0", price)
	case "F":
		clOrdID, orig, symbol, side :=
			get(msg, 11), get(msg, 41), get(msg, 55), get(msg, 54)
		fmt.Printf("ACCEPTOR: CANCEL_RX 11=%s 41=%s 55=%s 54=%s\n", clOrdID, orig, symbol, side)
		fmt.Printf("ACCEPTOR: CANCEL_RAW %s\n", raw)
		// Cancel confirm: nothing filled (CumQty 0, LeavesQty 0, AvgPx 0
		// — all required by the FIX44 dictionary).
		a.sendER(sid, clOrdID, orig, side, "4", "4", "", "", "", "0", "0", "0")
	case "G":
		clOrdID, orig, symbol, side, qty, price :=
			get(msg, 11), get(msg, 41), get(msg, 55), get(msg, 54), get(msg, 38), get(msg, 44)
		fmt.Printf("ACCEPTOR: REPLACE_RX 11=%s 41=%s 55=%s 54=%s 38=%s 44=%s\n", clOrdID, orig, symbol, side, qty, price)
		fmt.Printf("ACCEPTOR: REPLACE_RAW %s\n", raw)
		a.sendER(sid, clOrdID, orig, side, "5", "5", price, "", "", "0", qty, price)
	default:
		fmt.Printf("ACCEPTOR: APP_RX mt=%s raw=%s\n", mt, raw)
	}
	return nil
}

func main() {
	port := flag.Int("port", 0, "listen port (required)")
	sender := flag.String("sender", "REMOTE", "acceptor SenderCompID")
	target := flag.String("target", "FIXLAB", "expected initiator SenderCompID")
	dict := flag.String("dict", "backend/specs/FIX44.xml", "path to FIX44 data dictionary")
	stay := flag.Duration("stay", 0, "stop after this long (0 = run until killed)")
	useTLS := flag.Bool("tls", false, "terminate TLS on the listen port (native QuickFIX/Go TLS, TLS 1.2+)")
	certFile := flag.String("cert", "", "PEM certificate file for --tls (generated self-signed when empty)")
	keyFile := flag.String("key", "", "PEM private key file for --tls (generated self-signed when empty)")
	flag.Parse()

	if *port == 0 {
		fmt.Fprintln(os.Stderr, "fixacceptor: --port is required")
		os.Exit(2)
	}

	// --tls: native QuickFIX/Go server-side TLS. With no files given, a
	// self-signed certificate is generated into a temp dir (test only).
	tlsSettings := ""
	cleanupCert := func() {}
	if *useTLS {
		cf, kf := *certFile, *keyFile
		if cf == "" || kf == "" {
			cert, _, err := security.GenerateSelfSigned("localhost")
			if err != nil {
				fmt.Fprintln(os.Stderr, "fixacceptor: generate cert:", err)
				os.Exit(2)
			}
			certPEM, keyPEM, err := security.CertToPEM(cert)
			if err != nil {
				fmt.Fprintln(os.Stderr, "fixacceptor: cert PEM:", err)
				os.Exit(2)
			}
			dir, err := os.MkdirTemp("", "fixacceptor-tls")
			if err != nil {
				fmt.Fprintln(os.Stderr, "fixacceptor: temp dir:", err)
				os.Exit(2)
			}
			cleanupCert = func() { _ = os.RemoveAll(dir) }
			cf, kf = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
			if err := os.WriteFile(cf, certPEM, 0600); err != nil {
				fmt.Fprintln(os.Stderr, "fixacceptor: write cert:", err)
				os.Exit(2)
			}
			if err := os.WriteFile(kf, keyPEM, 0600); err != nil {
				fmt.Fprintln(os.Stderr, "fixacceptor: write key:", err)
				os.Exit(2)
			}
		}
		tlsSettings = fmt.Sprintf("SocketUseSSL=Y\nSocketCertificateFile=%s\nSocketPrivateKeyFile=%s\n", cf, kf)
	}
	defer cleanupCert()

	settingsText := fmt.Sprintf(`[DEFAULT]
ConnectionType=acceptor
SocketAcceptPort=%d
SocketAcceptHost=127.0.0.1
%sBeginString=FIX.4.4
SenderCompID=%s
TargetCompID=%s
DataDictionary=%s
[SESSION]
BeginString=FIX.4.4
SenderCompID=%s
TargetCompID=%s
`, *port, tlsSettings, *sender, *target, *dict, *sender, *target)

	settings, err := quickfix.ParseSettings(strings.NewReader(settingsText))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixacceptor: settings:", err)
		os.Exit(2)
	}
	a := &app{loggedOn: make(chan struct{}), sender: *sender, target: *target}
	acceptor, err := quickfix.NewAcceptor(a, quickfix.NewMemoryStoreFactory(), settings, quickfix.NewNullLogFactory())
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixacceptor: acceptor:", err)
		os.Exit(2)
	}
	if err := acceptor.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "fixacceptor: start:", err)
		os.Exit(2)
	}
	defer acceptor.Stop()
	fmt.Printf("ACCEPTOR: listening on 127.0.0.1:%d as %s->%s\n", *port, *sender, *target)

	if *stay > 0 {
		time.Sleep(*stay)
		return
	}
	select {} // run until killed
}
