// Command vps-probe-server receives agent reports over encrypted UDP, keeps
// everything in one SQLite file and serves a read-only web API.
package main

import (
	"flag"

	"github.com/shakespark/vps-probe/internal/cli"
)

var version = "dev"

const usage = `usage: vps-probe-server COMMAND [-config FILE] [flags]

Run:
  run [-debug]             run the server
  check                    validate the config and exit
  backup -o FILE           write a consistent copy of the database
  test-notify              send a test message to every notify channel

Nodes:
  add-node -id ID [-name NAME] [-ping-addr HOST] [-region R] [-group G]
                           add a node to the config and print the command that installs its agent
  install-cmd -node ID     print that command again: one line to run as root on the node
  install-cmd -upgrade     print a command that only upgrades an agent and keeps its config;
                           no token in it, the same for every node
  agent-config -node ID -o FILE
                           write the node's agent.yml instead ("-o -" prints it)
  remove-node -id ID       take a node out of the config and print what is left to do
      add-node, install-cmd and agent-config take -server HOST:PORT, how agents reach this
      server (default: public_addr in the config); the first two also -version V, -base URL
      and -signers FILE to choose the release the command installs

Helpers:
  gen-token                print a new random token
  hash-password            read a password, print its hash for basic_auth
  version
`

func main() {
	cli.Main(version, usage,
		cli.Command{Name: "run", Run: run},
		cli.Command{Name: "check", Run: check},
		cli.Command{Name: "backup", Run: backup},
		cli.Command{Name: "test-notify", Run: testNotify},
		cli.Command{Name: "add-node", Run: addNode},
		cli.Command{Name: "remove-node", Run: removeNode},
		cli.Command{Name: "install-cmd", Run: installCmd},
		cli.Command{Name: "agent-config", Run: agentConfig},
		cli.Command{Name: "gen-token", Run: genToken},
		cli.Command{Name: "hash-password", Run: hashPassword},
	)
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "/etc/vps-probe/server.yml", "config file")
}
