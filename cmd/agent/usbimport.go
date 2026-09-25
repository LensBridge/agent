package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/LensBridge/agent/internal/config"
	"github.com/LensBridge/agent/internal/notice"
	"github.com/LensBridge/agent/internal/usbimport"
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

	cfg, err := config.Load(defaultConfigPath)
	if err != nil {
		// Not enrolled (or config unreadable): there is no daemon importing
		// yet, so there is nothing to hand the stick to. Not an error for
		// the unit; the stick is simply ignored.
		fmt.Printf("USB import: ignoring %s: this board is not set up yet (%v)\n", args[0], err)
		return
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
