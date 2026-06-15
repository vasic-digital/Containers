// cmd/applesim — Apple iOS/iPadOS/tvOS Simulator detection + lifecycle CLI.
//
// Thin command wrapper over pkg/applesim so consuming projects (and their thin
// glue scripts) can detect an Xcode/simctl install, enumerate simulator
// devices, and boot/install/launch/record/shutdown them from one binary.
//
// Apple Simulator runtimes are macOS-host-only and CANNOT run inside a Linux
// container (unlike pkg/emulator's Android-in-container or pkg/genymotion's
// Android-in-VM), so this wraps the host-native `xcrun simctl` toolchain — the
// only viable backend for iOS simulators.
//
// Usage:
//
//	applesim detect                       # print xcrun path or exit 2 if unavailable
//	applesim version                      # print Xcode version
//	applesim list                         # list every simulator (tab-separated)
//	applesim running                      # list only booted simulators
//	applesim resolve <udid|name>          # print the UDID of a matching device
//	applesim boot <udid|name>             # boot a device and wait until booted
//	applesim install <udid|name> <app>    # install an .app bundle
//	applesim launch <udid|name> <bundle>  # launch an installed app by bundle id
//	applesim screenshot <udid|name> <png> # capture a screenshot
//	applesim shutdown <udid|name>         # shut a device down
//
// Exit codes: 0 success; 1 runtime failure; 2 xcrun/simctl unavailable or usage error.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"digital.vasic.containers/pkg/applesim"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// resolveUDID maps a UDID-or-name argument to a concrete UDID via the package's
// stable-identifier resolver (§11.4.111 — never an enumeration index).
func resolveUDID(ctx context.Context, tool *applesim.Tool, idOrName string) (string, error) {
	d, err := tool.Resolve(ctx, idOrName)
	if err != nil {
		return "", err
	}
	return d.UDID, nil
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: applesim <detect|version|list|running|resolve|boot|install|launch|screenshot|shutdown> [args]")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tool, err := applesim.Detect(ctx)
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
		var devices []applesim.Device
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
			fmt.Printf("%s\t%s\t%s\t%s\n", d.State, d.UDID, d.Name, d.Runtime)
		}
		return 0
	case "resolve":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: applesim resolve <udid|name>")
			return 2
		}
		udid, err := resolveUDID(ctx, tool, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(udid)
		return 0
	case "boot":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: applesim boot <udid|name>")
			return 2
		}
		udid, err := resolveUDID(ctx, tool, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		d, err := tool.BootAndWait(ctx, udid, 3*time.Minute)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(d.UDID)
		return 0
	case "install":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: applesim install <udid|name> <app-path>")
			return 2
		}
		udid, err := resolveUDID(ctx, tool, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := tool.Install(ctx, udid, args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "launch":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: applesim launch <udid|name> <bundle-id>")
			return 2
		}
		udid, err := resolveUDID(ctx, tool, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		out, err := tool.Launch(ctx, udid, args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(out)
		return 0
	case "screenshot":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: applesim screenshot <udid|name> <out.png>")
			return 2
		}
		udid, err := resolveUDID(ctx, tool, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := tool.Screenshot(ctx, udid, args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case "shutdown":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: applesim shutdown <udid|name>")
			return 2
		}
		udid, err := resolveUDID(ctx, tool, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := tool.Shutdown(ctx, udid); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		return 2
	}
}
