// Command wa-evidence-collector collects WhatsApp group messages as
// evidence on a built-in schedule.
//
// It manages the bridge lifecycle (optional autostart + health wait) and
// runs the collector either once (--once) or on the schedules from the
// config file.
//
// Usage:
//
//	wa-evidence-collector --config config.yaml          # scheduled mode
//	wa-evidence-collector --config config.yaml --once   # one run, then exit
//
// The collector itself is read-only against the bridge store; see the
// collector package docs and README.md for the privacy model.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wa-evidence-collector/wa-evidence-collector/collector"
	"github.com/wa-evidence-collector/wa-evidence-collector/scheduler"
)

const version = "v0.6.0"

func main() {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	once := flag.Bool("once", false, "run one collection and exit (no scheduler)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	log.SetFlags(log.LstdFlags)
	if *showVersion {
		fmt.Printf("wa-evidence-collector %s\n", version)
		os.Exit(0)
	}

	cfg, err := collector.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	coll, err := collector.New(cfg, log.Default())
	if err != nil {
		log.Fatalf("collector: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bridge lifecycle: start it when configured, then wait for health.
	var bridgeCmd *exec.Cmd
	if cfg.Bridge.Autostart {
		bridgeCmd, err = startBridge(cfg.Bridge.Binary)
		if err != nil {
			log.Printf("warning: could not start bridge %q: %v (continuing; DB reads may still work, media will fail)", cfg.Bridge.Binary, err)
		} else {
			defer stopBridge(bridgeCmd)
			waitBridgeHealthy(ctx, coll, cfg.Bridge.HealthTimeoutSeconds)
		}
	} else {
		waitBridgeHealthy(ctx, coll, cfg.Bridge.HealthTimeoutSeconds)
	}

	if *once {
		until := time.Now().UTC()
		since := until.Add(-time.Duration(cfg.Collection.LookbackHours) * time.Hour)
		sum, err := coll.Collect(ctx, "once", since, until)
		if err != nil {
			log.Fatalf("collect: %v", err)
		}
		printSummary(sum)
		return
	}

	sched := scheduler.New(log.Default())
	for _, entry := range cfg.Schedules {
		e := entry // capture
		if err := sched.Add(e.Spec, func(ctx context.Context) error {
			until := time.Now().UTC()
			since := until.Add(-time.Duration(e.WindowHours) * time.Hour)
			sum, err := coll.Collect(ctx, e.Label, since, until)
			if err != nil {
				return err
			}
			printSummary(sum)
			return nil
		}); err != nil {
			log.Fatalf("schedule %q: %v", e.Label, err)
		}
		log.Printf("scheduled %q: %q (window %dh)", e.Label, e.Spec, e.WindowHours)
	}
	if len(cfg.Schedules) == 0 {
		log.Printf("warning: no schedules in config; running idle (use --once for a single run)")
	}
	sched.Start()
	log.Printf("wa-evidence-collector %s running (Ctrl+C to stop)", version)
	<-ctx.Done()
	log.Printf("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sched.Stop(shutdownCtx)
	log.Printf("stopped")
}

// startBridge launches the bridge binary as a child process. The bridge
// resolves its store/ directory relative to its working directory, so it
// runs with the binary's own directory as cwd.
func startBridge(binary string) (*exec.Cmd, error) {
	abs, err := filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(abs)
	cmd.Dir = filepath.Dir(abs)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	log.Printf("bridge started (pid %d)", cmd.Process.Pid)
	return cmd, nil
}

func stopBridge(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	log.Printf("bridge stopped")
}

// waitBridgeHealthy polls /api/health until it answers 200 or the timeout
// elapses. A 503 (bridge up, WhatsApp disconnected) is not fatal: local
// store reads still work, only media downloads fail. A timeout is a
// warning, not a fatal error, for the same reason.
func waitBridgeHealthy(ctx context.Context, coll *collector.Collector, timeoutSeconds int) {
	if timeoutSeconds <= 0 {
		return
	}
	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	for {
		healthy, err := coll.Bridge().Health(ctx)
		if err == nil && healthy {
			log.Printf("bridge healthy")
			return
		}
		if err == nil && !healthy {
			log.Printf("bridge up but WhatsApp disconnected (reads still work; media downloads will fail)")
			return
		}
		if time.Now().After(deadline) {
			log.Printf("warning: bridge health check timed out after %ds (%v); continuing in degraded mode", timeoutSeconds, err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func printSummary(sum *collector.Summary) {
	log.Printf("collect [%s] window %s..%s: scanned=%d new=%d skipped-seen=%d merged=%d similar=%d agenda=%d complete=%d media ok=%d failed=%d skipped=%d renders=%d render-failed=%d upload ok=%d failed=%d skipped=%d dir=%s",
		sum.Label,
		sum.WindowStart.Format(time.RFC3339), sum.WindowEnd.Format(time.RFC3339),
		sum.Scanned, sum.New, sum.SkippedSeen, sum.Merged, sum.SimilarNotes,
		sum.Agenda, sum.AgendaComplete,
		sum.MediaOK, sum.MediaFailed, sum.MediaSkipped, sum.Renders, sum.RenderFailed,
		sum.UploadOK, sum.UploadFailed, sum.UploadSkipped, sum.RunDir)
	for _, f := range sum.Files {
		log.Printf("  wrote %s", f)
	}
	for local, url := range sum.UploadedFiles {
		log.Printf("  uploaded %s -> %s", local, url)
	}
}
