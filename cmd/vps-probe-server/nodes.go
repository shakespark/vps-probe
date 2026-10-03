package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shakespark/vps-probe/internal/atomicfile"
	"github.com/shakespark/vps-probe/internal/release"
	"github.com/shakespark/vps-probe/internal/server/config"
)

// agentFlags say how to make a node's agent.yml: where agents report to.
type agentFlags struct{ server *string }

func addAgentFlags(fs *flag.FlagSet) agentFlags {
	return agentFlags{fs.String("server", "", "address agents use to reach this server, host:port (default: public_addr in the config)")}
}

// config renders node's agent.yml.
func (f agentFlags) config(cfg *config.Config, node string) (string, error) {
	server := *f.server
	if server == "" {
		server = cfg.PublicAddr
	}
	if server == "" {
		return "", errors.New("pass -server HOST:PORT (how agents reach this server), or set public_addr in the config")
	}
	return cfg.AgentConfig(node, server)
}

// releaseFlags choose the release an install command installs.
type releaseFlags struct{ version, base, signers *string }

func addReleaseFlags(fs *flag.FlagSet) releaseFlags {
	return releaseFlags{
		version: fs.String("version", "", "release the command installs (default: this server's version)"),
		base:    fs.String("base", "", "release base URL, e.g. a mirror (default: "+release.DefaultBase+")"),
		signers: fs.String("signers", "", "allowed_signers file to verify the release with (default: the built-in release keys)"),
	}
}

// command returns the line that installs the release with agentConfig, or
// upgrades to it when agentConfig is empty.
func (f releaseFlags) command(agentConfig string) (string, error) {
	c := release.Command{Version: *f.version, Base: *f.base, AgentConfig: agentConfig, Upgrade: agentConfig == ""}
	if c.Version == "" {
		c.Version = version
	}
	if *f.signers != "" {
		b, err := os.ReadFile(*f.signers)
		if err != nil {
			return "", err
		}
		c.Signers = string(b)
	}
	return c.Line()
}

func installCmd(args []string) error {
	fs := flag.NewFlagSet("install-cmd", flag.ExitOnError)
	path := configFlag(fs)
	node := fs.String("node", "", "node id from server.yml")
	upgrade := fs.Bool("upgrade", false, "print a command that only upgrades the agent and keeps the config on the VPS")
	af, rf := addAgentFlags(fs), addReleaseFlags(fs)
	fs.Parse(args)
	if *upgrade {
		// Nothing node-specific: the config file is not even read.
		if *node != "" || *af.server != "" {
			return errors.New("install-cmd: -upgrade keeps each VPS's own agent.yml, so it takes no -node or -server; " +
				"to rewrite a node's config, run install-cmd -node ID without -upgrade")
		}
		line, err := rf.command("")
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, "Run this as root on any VPS that already has the agent. It replaces the programs and keeps\n"+
			"/etc/vps-probe/agent.yml as it is. Or: vps-probe-server install-cmd -upgrade | ssh root@THAT_VPS sh\n\n")
		fmt.Println(line)
		return nil
	}
	if *node == "" {
		return errors.New("install-cmd: pass -node ID, or -upgrade")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	text, err := af.config(cfg, *node)
	if err != nil {
		return err
	}
	line, err := rf.command(text)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Run this as root on %s. It contains the node's token; to keep it out of every shell\n"+
		"history, pipe it instead: vps-probe-server install-cmd -node %s | ssh root@THAT_VPS sh\n\n", *node, *node)
	fmt.Println(line)
	return nil
}

// addNode inserts a node into the config file and prints what to do next.
// The file is replaced only if the result is a valid config; the previous
// version stays beside it as a .bak file.
func addNode(args []string) error {
	fs := flag.NewFlagSet("add-node", flag.ExitOnError)
	path := configFlag(fs)
	n := config.NewNode{}
	fs.StringVar(&n.ID, "id", "", "node id: letters, digits, '.', '_' and '-'")
	fs.StringVar(&n.Name, "name", "", "display name (default: the id)")
	fs.StringVar(&n.PingAddr, "ping-addr", "", "address other nodes ping, IP or domain (default: none, nobody pings it)")
	fs.StringVar(&n.Region, "region", "", "region code shown as a badge, e.g. HK")
	fs.StringVar(&n.Group, "group", "", "group for the overview tabs")
	af, rf := addAgentFlags(fs), addReleaseFlags(fs)
	fs.Parse(args)
	if n.ID == "" {
		return errors.New("add-node: -id is required")
	}
	n.Token = newToken()

	// Load also checks the file's permissions.
	if _, err := config.Load(*path); err != nil {
		return err
	}
	old, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	data, err := config.AddNode(old, n)
	if err != nil {
		return err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return err
	}
	// Before touching the file: a command that cannot be printed (no
	// -server, a dev build without -version) should not leave a node behind.
	text, err := af.config(cfg, n.ID)
	if err != nil {
		return err
	}
	line, err := rf.command(text)
	if err != nil {
		return err
	}
	bak, err := keepCopy(*path, old)
	if err != nil {
		return err
	}
	if err := atomicfile.Replace(*path, data); err != nil {
		return err
	}

	w := os.Stderr
	fmt.Fprintf(w, "Added node %s to %s (previous version: %s).\n\n", n.ID, *path, bak)
	fmt.Fprintf(w, "1. Restart the server so it accepts the node:\n\n     systemctl restart vps-probe-server\n\n")
	fmt.Fprintf(w, "2. Right after that, run this as root on the new VPS. It contains the node's token; to keep\n"+
		"   it out of every shell history, pipe it instead:\n"+
		"     vps-probe-server install-cmd -node %s | ssh root@THAT_VPS sh\n\n", n.ID)
	fmt.Println(line)
	var others []string
	for _, p := range cfg.Nodes {
		if p.ID != n.ID && n.PingAddr != "" && !cfg.PingExcluded(n.ID, p.ID) {
			others = append(others, p.ID)
		}
	}
	if len(others) > 0 {
		fmt.Fprintf(w, "\n3. Optional: for the other nodes to ping %s, reinstall their config. For each of\n   %s:\n\n"+
			"     vps-probe-server install-cmd -node ID     # run the printed command on that VPS\n",
			n.ID, strings.Join(others, ", "))
	}
	return nil
}

// keepCopy saves content, the current content of path, beside it under a
// new name, readable by root only, and returns that name.
func keepCopy(path string, content []byte) (string, error) {
	name := path + ".bak-" + time.Now().Format("20060102-150405")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // never over an existing copy
	if err != nil {
		return "", err
	}
	_, err = f.Write(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return name, err
}

func agentConfig(args []string) error {
	fs := flag.NewFlagSet("agent-config", flag.ExitOnError)
	path := configFlag(fs)
	node := fs.String("node", "", "node id from server.yml")
	out := fs.String("o", "", "output file, written with mode 0600 (\"-\" for stdout)")
	af := addAgentFlags(fs)
	fs.Parse(args)
	if *node == "" || *out == "" {
		return errors.New("agent-config: -node and -o are required")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	text, err := af.config(cfg, *node)
	if err != nil {
		return err
	}
	if *out == "-" {
		_, err = os.Stdout.WriteString(text)
		return err
	}
	if err := atomicfile.Write(*out, []byte(text), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (contains the token for %s; copy it to that VPS and delete it here)\n", *out, *node)
	return nil
}
