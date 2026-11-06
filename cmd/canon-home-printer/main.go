// Command canon-home-printer serves the family website for the Canon printer at home:
// print, scan, see what the printer is doing, and run cleanings.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/taktekhq/canon-home-printer/internal/auth"
	"github.com/taktekhq/canon-home-printer/internal/config"
	"github.com/taktekhq/canon-home-printer/internal/events"
	"github.com/taktekhq/canon-home-printer/internal/office"
	"github.com/taktekhq/canon-home-printer/internal/printer"
	"github.com/taktekhq/canon-home-printer/internal/scanner"
	"github.com/taktekhq/canon-home-printer/internal/server"
	"github.com/taktekhq/canon-home-printer/web"
)

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "canon-home-printer %s: family website for a Canon printer at home.\n"+
			"Configure it with environment variables; see deploy/canon-home-printer.env.example.\n", version)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	p := printer.New(cfg.CUPSHost, cfg.CUPSPort)
	p.Queue, p.MaintQueue = cfg.Queue, cfg.MaintQueue
	p.USBVendor, p.USBProduct = cfg.USBVendor, cfg.USBProduct
	p.Computer, p.Admin, p.CUPSLog = cfg.ComputerName, cfg.AdminName, cfg.CUPSLog

	srv := &server.Server{
		Cfg:     cfg,
		Printer: p,
		Scanner: &scanner.Scanner{Device: cfg.Scanner, Dir: cfg.ScanDir, Computer: cfg.ComputerName, Admin: cfg.AdminName},
		Events:  &events.Log{Path: cfg.EventLog},
		Static:  web.Files,
		Token:   auth.Token,
	}
	if conv := (&office.Converter{Command: os.Getenv("CHP_SOFFICE"), TempDir: cfg.UploadDir}); conv.Available() {
		srv.Office = conv
		log.Printf("Word, Excel and PowerPoint files are converted with LibreOffice")
	} else {
		log.Printf("LibreOffice (soffice) not found: Word and other office files are refused")
	}
	switch {
	case cfg.TeamDomain != "" && cfg.AUD != "":
		srv.Verifier = &auth.Verifier{TeamDomain: cfg.TeamDomain, AUD: cfg.AUD}
	case cfg.DevUser != "" && cfg.TeamDomain == "":
		log.Printf("WARNING: no Cloudflare Access configured; every request is treated as %s. Local testing only.", cfg.DevUser)
	default:
		log.Printf("Cloudflare Access isn't configured (CF_ACCESS_TEAM_DOMAIN, CF_ACCESS_AUD): refusing every request.")
	}

	stop := make(chan struct{})
	go srv.Cleanup(stop)

	hs := &http.Server{Addr: cfg.Addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		log.Printf("canon-home-printer %s listening on %s (queue %s)", version, cfg.Addr, cfg.Queue)
		if err := hs.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hs.Shutdown(ctx)
}
