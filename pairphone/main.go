// PairPhone helper: generates an 8-character WhatsApp pairing code
// ("Link with phone number") using the same whatsmeow store as the bridge,
// so the bridge picks the session up after pairing.
//
// Usage:
//   pairphone <phone-international-digits, e.g. 62XXXXXXXXXX> <abs path to whatsapp.db>
//
// The bridge service must be STOPPED before running this (SQLite store lock).
// Prints PAIRING_CODE=<8 chars> as soon as it is generated, then waits up to
// ~150s for the phone to enter the code and prints PAIRED on success.
// The login websocket lives ~160s (QR codes run out), so the code must be
// entered quickly after it is printed.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: pairphone <phone-digits> <abs-path-to-whatsapp.db>")
		os.Exit(2)
	}
	phone := os.Args[1]
	dbPath := os.Args[2]
	ctx := context.Background()

	dbLog := waLog.Stdout("Database", "INFO", true)
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+dbPath+"?_foreign_keys=on", dbLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open store: %v\n", err)
		os.Exit(1)
	}
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read device: %v\n", err)
		os.Exit(1)
	}
	if deviceStore == nil || deviceStore.Deleted {
		deviceStore = container.NewDevice()
	} else if deviceStore.ID != nil {
		fmt.Fprintln(os.Stderr, "device already paired, nothing to do")
		os.Exit(3)
	}

	client := whatsmeow.NewClient(deviceStore, waLog.Stdout("Client", "INFO", true))
	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get QR channel: %v\n", err)
		os.Exit(1)
	}
	if err := client.Connect(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect: %v\n", err)
		os.Exit(1)
	}
	// Wait for the first QR channel item: connection fully established.
	select {
	case item := <-qrChan:
		if item.Event != "code" {
			fmt.Fprintf(os.Stderr, "unexpected first QR event: %s\n", item.Event)
			os.Exit(1)
		}
	case <-time.After(30 * time.Second):
		fmt.Fprintln(os.Stderr, "timed out waiting for connection")
		os.Exit(1)
	}

	code, err := client.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Mac OS)")
	if err != nil {
		fmt.Fprintf(os.Stderr, "PairPhone failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("PAIRING_CODE=%s\n", code)
	// Flush stdout so the caller sees the code immediately.
	os.Stdout.Sync()

	// Wait for the phone to enter the code (channel emits "success").
	timeout := time.After(150 * time.Second)
	for {
		select {
		case item, ok := <-qrChan:
			if !ok {
				fmt.Fprintln(os.Stderr, "QR channel closed without pairing")
				os.Exit(1)
			}
			if item.Event == "success" {
				// Give the client a moment to finish authenticating.
				time.Sleep(3 * time.Second)
				if client.IsConnected() {
					fmt.Println("PAIRED")
					return
				}
				fmt.Println("PAIRED")
				return
			}
			if item.Event == "timeout" {
				fmt.Fprintln(os.Stderr, "pairing timed out (code expired)")
				os.Exit(1)
			}
		case <-timeout:
			fmt.Fprintln(os.Stderr, "timed out waiting for code entry")
			os.Exit(1)
		}
	}
}
