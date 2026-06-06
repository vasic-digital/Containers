// cmd/genymotion — Genymotion virtual-device detection + lifecycle CLI.
//
// Thin command wrapper over pkg/genymotion so consuming projects (and their
// thin glue scripts) can detect a Genymotion install, enumerate virtual
// devices, and boot/stop them from one binary — the §6.AH "virtual devices in
// VMs, driven by the Containers submodule" path for Genymotion on macOS + Linux.
//
// Usage:
//
//	genymotion detect            # print gmtool path or exit 2 if not installed
//	genymotion version           # print Genymotion version
//	genymotion list              # list every virtual device (tab-separated)
//	genymotion running           # list only running devices
//	genymotion serial <name>     # print the adb serial of a running device
//	genymotion start <name>      # boot a device and wait until adb-ready
//	genymotion stop <name>       # stop a device
//
// Exit codes: 0 success; 1 runtime failure; 2 gmtool not installed / usage error.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"digital.vasic.containers/pkg/genymotion"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: genymotion <detect|version|list|running|serial|start|stop> [name]")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tool, err := genymotion.Detect()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	switch args[0] {
	case "detect":
		fmt.Println(tool.Path)
		return 0
	case "version":
		v, err := tool.Version(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(v)
		return 0
	case "list", "running":
		var devices []genymotion.Device
		if args[0] == "running" {
			devices, err = tool.Running(ctx)
		} else {
			devices, err = tool.List(ctx)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, d := range devices {
			fmt.Printf("%s\t%s\t%s\t%s\n", d.State, d.ADBSerial, d.UUID, d.Name)
		}
		return 0
	case "serial":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: genymotion serial <name>")
			return 2
		}
		devices, err := tool.Running(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, d := range devices {
			if strings.EqualFold(d.Name, args[1]) || d.UUID == args[1] {
				fmt.Println(d.ADBSerial)
				return 0
			}
		}
		fmt.Fprintf(os.Stderr, "genymotion: no running device named %q\n", args[1])
		return 1
	case "start":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: genymotion start <name>")
			return 2
		}
		d, err := tool.StartAndWait(ctx, args[1], 3*time.Minute)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(d.ADBSerial)
		return 0
	case "stop":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: genymotion stop <name>")
			return 2
		}
		if err := tool.Stop(ctx, args[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		return 2
	}
}
