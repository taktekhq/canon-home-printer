// Package config reads settings from environment variables (see deploy/canon-home-printer.env.example).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds every setting.
type Config struct {
	Addr string // where to listen; keep it on 127.0.0.1 behind the tunnel

	PrinterName string // shown in the page title, e.g. "Canon G3010"
	Queue       string // CUPS queue used for printing
	MaintQueue  string // CUPS queue that takes raw maintenance bytes; defaults to Queue
	Scanner     string // SANE device name for scanimage
	USBVendor   string // lowercase hex, for "is it plugged in?"
	USBProduct  string
	CUPSHost    string
	CUPSPort    int
	CUPSLog     string

	ComputerName string // how the family calls the machine the printer is plugged into
	AdminName    string // who to ask when something needs fixing

	DataDir    string
	UploadDir  string
	ScanDir    string
	EventLog   string
	NamesFile  string
	MaxUpload  int64 // bytes
	UploadDays int
	ScanDays   int

	TeamDomain string   // Cloudflare Access team domain, e.g. myteam.cloudflareaccess.com
	AUD        string   // the Access application's AUD tag
	DevUser    string   // local testing only: trust this email when TeamDomain is empty
	MaintUsers []string // emails allowed to run cleanings; empty = everyone signed in
}

func env(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) (int, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", name, v)
	}
	return n, nil
}

// Load reads the environment and creates the data folders.
func Load() (*Config, error) {
	home, _ := os.UserHomeDir()
	c := &Config{
		Addr:         env("CHP_ADDR", "127.0.0.1:8091"),
		PrinterName:  env("CHP_PRINTER_NAME", "Canon G3010"),
		Queue:        env("CHP_QUEUE", "Canon_G3010"),
		Scanner:      env("CHP_SCANNER", ""),
		USBVendor:    strings.ToLower(env("CHP_USB_VENDOR", "04a9")),
		USBProduct:   strings.ToLower(env("CHP_USB_PRODUCT", "183b")),
		CUPSHost:     env("CHP_CUPS_HOST", "localhost"),
		CUPSLog:      env("CHP_CUPS_LOG", "/var/log/cups/error_log"),
		ComputerName: env("CHP_COMPUTER_NAME", "the computer"),
		AdminName:    env("CHP_ADMIN_NAME", "the person who set up the printer"),
		DataDir:      env("CHP_DATA_DIR", filepath.Join(home, ".local/share/canon-home-printer")),
		TeamDomain:   env("CF_ACCESS_TEAM_DOMAIN", ""),
		AUD:          env("CF_ACCESS_AUD", ""),
		DevUser:      strings.ToLower(env("CHP_DEV_USER", "")),
	}
	c.MaintQueue = env("CHP_MAINT_QUEUE", c.Queue)
	var err error
	if c.CUPSPort, err = envInt("CHP_CUPS_PORT", 631); err != nil {
		return nil, err
	}
	mb, err := envInt("CHP_MAX_UPLOAD_MB", 50)
	if err != nil {
		return nil, err
	}
	c.MaxUpload = int64(mb) << 20
	if c.UploadDays, err = envInt("CHP_UPLOAD_KEEP_DAYS", 7); err != nil {
		return nil, err
	}
	if c.ScanDays, err = envInt("CHP_SCAN_KEEP_DAYS", 30); err != nil {
		return nil, err
	}
	for _, e := range strings.Split(env("CHP_MAINT_USERS", ""), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			c.MaintUsers = append(c.MaintUsers, e)
		}
	}
	c.UploadDir = filepath.Join(c.DataDir, "uploads")
	c.ScanDir = filepath.Join(c.DataDir, "scans")
	c.EventLog = filepath.Join(c.DataDir, "events.jsonl")
	c.NamesFile = env("CHP_NAMES", filepath.Join(c.DataDir, "names.json"))
	for _, d := range []string{c.UploadDir, c.ScanDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return c, nil
}
