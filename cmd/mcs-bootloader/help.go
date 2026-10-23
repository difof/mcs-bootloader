package main

import (
	"fmt"
	"strings"
	"text/tabwriter"
)

type helpCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

var helpCommands = []helpCommand{
	{"init [PATH]", "Create a private .env from the embedded example, with a random RCON password."},
	{"config new [PATH]", "Create a new .env (same as init; refuses overwriting files)."},
	{"config link [PATH]", "Link a local .env to the installed canonical configuration."},
	{"config edit", "Open the resolved .env in VISUAL, EDITOR, or nano."},
	{"config show", "Show validated disk settings with secrets redacted."},
	{"config check", "Validate .env syntax, values and private file permissions."},
	{"config live", "Show the gateway's currently applied settings (redacted)."},
	{"config reload [PATH]", "Apply live .env changes atomically through the control socket."},
	{"install [--report PATH]", "Install this executable, service units and the private configuration."},
	{"deploy", "Replace the installed executable atomically, retaining the previous binary."},
	{"remove [--dry-run | --yes]", "Preview removal; --yes removes owned service files with Minecraft stopped, preserving data."},
	{"status", "Show server lifecycle, players, traffic and applied config metadata."},
	{"stats", "Show a compact snapshot of cached gateway, Minecraft, JVM and directory metrics."},
	{"start / wake", "Start Minecraft through the gateway (returns while it boots)."},
	{"stop / sleep", "Request a safe RCON stop; refuses active/unknown players or pending joins."},
	{"command [--timeout 30s] -- COMMAND ...", "Send one literal Minecraft operator command; never retries or starts Minecraft."},
	{"gateway start | stop | restart | configure", "Manage the gateway service; its stop/restart leaves Java running."},
	{"logs [--server] [--follow] [--lines N]", "Show gateway or Minecraft journal entries; --json streams JSON records."},
	{"metrics [--raw]", "Show cached Prometheus samples; --raw prints scrape text, --json returns structured samples."},
	{"serve", "Run the gateway in the foreground (normally managed by systemd)."},
	{"version", "Show the executable version."},
}

func printHelp(o cliOptions, out *cliOutput) error {
	topic := o.Command
	if topic == "help" {
		topic = strings.Join(o.Args, " ")
	}
	if topic == "help" {
		topic = ""
	}
	entries := make([]helpCommand, 0)
	for _, entry := range helpCommands {
		if topic == "" || strings.HasPrefix(entry.Command, topic) {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		entries = helpCommands
	}
	flags := []helpCommand{
		{"--env PATH", "Use this .env; otherwise ./.env, then /etc/mcs-bootloader/.env."},
		{"--json", "Emit machine-readable JSON; errors are JSON on stderr; logs are NDJSON."},
		{"--color auto|always|never", "Color human output (auto honors terminals and NO_COLOR)."},
		{"--socket PATH", "Use a specific private gateway socket for recovery or automation."},
		{"--help, -h", "Show command help."},
	}
	if out.JSON {
		return writeJSON(out.Out, map[string]any{"name": "mcs-bootloader", "version": version, "usage": "mcs-bootloader [global flags] COMMAND [flags] [arguments]", "commands": entries, "global_flags": flags})
	}
	fmt.Fprintln(out.Out, out.color(out.Out, "1;36", "mcs-bootloader"), version)
	fmt.Fprintln(out.Out, "Minecraft on demand, with safe shutdown and opaque mod traffic forwarding.")
	fmt.Fprintln(out.Out, "\nUsage: mcs-bootloader [global flags] COMMAND [flags] [arguments]")
	fmt.Fprintln(out.Out, "\nCommands")
	w := tabwriter.NewWriter(out.Out, 0, 4, 2, ' ', 0)
	for _, entry := range entries {
		fmt.Fprintf(w, "  %s\t%s\n", entry.Command, entry.Description)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out.Out, "\nGlobal flags")
	w = tabwriter.NewWriter(out.Out, 0, 4, 2, ' ', 0)
	for _, flag := range flags {
		fmt.Fprintf(w, "  %s\t%s\n", flag.Command, flag.Description)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out.Out, "\nExamples:\n  mcs-bootloader init\n  mcs-bootloader --env .env config edit\n  mcs-bootloader status --json\n  mcs-bootloader command -- op PlayerName\n  mcs-bootloader config reload /etc/mcs-bootloader/.env\n  mcs-bootloader logs --server --follow")
	return nil
}
