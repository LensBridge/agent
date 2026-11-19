package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/LensBridge/agent/internal/core/config"
	"github.com/LensBridge/agent/internal/core/notice"
	"github.com/LensBridge/agent/internal/core/usbimport"
)

// runUSBImport is `usb-import <kernel name>` (docs/architecture.md, section
// 9.6), started as root by musallahboard-usb-import@<name>.service when a
// USB stick is plugged in. Everything it prints lands in the journal.
func runUSBImport(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: musallahboard-agent usb-import <kernel device name, e.g. sda1>")
		os.Exit(2)
	}
	requireRoot("usb-import")

	// An unprovisioned board (no agent.toml) takes sticks too: its setup
	// file comes on one. An unreadable config does not stop a stick either;
	// the daemon decides what to install.
	cfg, err := config.Load(defaultConfigPath)
	if err != nil {
		fmt.Printf("USB import: the agent config is unreadable (%v); reading %s anyway\n", err, args[0])
		cfg = &config.Config{}
	}
	if !cfg.USBImport() {
		fmt.Printf("USB import: ignoring %s: usb_import is off in the agent config\n", args[0])
		usbimport.New().Announce(args[0], notice.New(notice.Neutral, "USB updates are turned off",
			"This board is set up not to read USB sticks"))
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	err = usbimport.New().Run(ctx, args[0])
	switch {
	case errors.Is(err, usbimport.ErrUnsupported):
		// Already explained; a stick we do not read is not a failure.
	case err != nil:
		fmt.Printf("USB import from %s failed: %v\n", args[0], err)
		os.Exit(1)
	}
}
