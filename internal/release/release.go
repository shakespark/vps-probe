// Package release holds the release signing keys and builds the one-line
// command that installs a signed release on a VPS (docs/DESIGN.md §6.1.1).
package release

import (
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Signers is the allowed_signers file releases are verified against. It is
// compiled into the server so the install command can carry the keys.
//
//go:embed release-signers
var Signers string

const (
	// DefaultBase is where releases are published: <base>/v<version>/<file>.
	DefaultBase = "https://github.com/shakespark/vps-probe/releases/download"
	// Namespace of release signatures (ssh-keygen -n).
	Namespace = "vps-probe-release"
)

// Command describes one install command.
type Command struct {
	Version     string // release to install, without the leading v
	Base        string // release base URL; DefaultBase if empty
	Signers     string // allowed_signers content; the built-in keys if empty
	AgentConfig string // the node's agent.yml
	// Upgrade makes a command that replaces the programs and keeps the
	// agent.yml already on the machine. It carries no config and no token,
	// so one command serves every node.
	Upgrade bool
}

var (
	versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	// What may appear unquoted inside the command: no quote, space or
	// shell metacharacter.
	baseRE      = regexp.MustCompile(`^https?://[A-Za-z0-9._~:/@%+=-]+$`)
	principalRE = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)
)

// signerLines returns the key lines of an allowed_signers file and the
// principal they share.
func signerLines(s string) (lines []string, principal string, err error) {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		p := strings.Fields(l)[0]
		if principal != "" && p != principal {
			return nil, "", fmt.Errorf("release signers: principals %q and %q differ; use one name for every key", principal, p)
		}
		if !principalRE.MatchString(p) {
			return nil, "", fmt.Errorf("release signers: principal %q: want letters, digits and . _ @ -", p)
		}
		principal = p
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return nil, "", errors.New("release signers: no key")
	}
	return lines, principal, nil
}

// Line returns the command: one line of POSIX sh to run as root on the VPS.
// It downloads the release for this machine's CPU, verifies its signature
// and checksum, and runs the release's own install.sh with the embedded
// agent.yml (an upgrade command embeds none and runs install.sh --upgrade).
// Nothing unverified is executed. Downloads are bounded, so a machine that
// cannot reach the release host fails with a message instead of hanging.
// The line contains the node's token; it starts with a space, which keeps
// it out of the shell history where HISTCONTROL=ignorespace is set (root on
// Debian does not set it).
func (c Command) Line() (string, error) {
	if !versionRE.MatchString(c.Version) {
		return "", fmt.Errorf("version %q: want a release like 0.2.0 (pass -version)", c.Version)
	}
	base := strings.TrimRight(c.Base, "/")
	if base == "" {
		base = DefaultBase
	}
	if !baseRE.MatchString(base) {
		return "", fmt.Errorf("release base URL %q: want an http(s) URL without spaces or quotes", c.Base)
	}
	signers := c.Signers
	if signers == "" {
		signers = Signers
	}
	lines, principal, err := signerLines(signers)
	if err != nil {
		return "", err
	}
	if c.Upgrade && c.AgentConfig != "" {
		return "", errors.New("an upgrade command keeps the installed config and cannot carry one")
	}
	if !c.Upgrade && c.AgentConfig == "" {
		return "", errors.New("empty agent config")
	}
	b64 := base64.StdEncoding.EncodeToString
	// Through sh, not by its executable bit: where the temporary directory is
	// mounted noexec the installer still starts, and says so.
	install := `echo ` + b64([]byte(c.AgentConfig)) + ` | base64 -d > agent.yml; sh ./$P/install.sh agent --config agent.yml`
	if c.Upgrade {
		install = `sh ./$P/install.sh agent --upgrade`
	}

	// Inside single quotes: the script itself must not contain one. Data
	// (keys, config) travels as base64, so its content cannot matter.
	script := strings.Join([]string{
		`set -eu`,
		`umask 077`,
		`V=` + c.Version,
		`B=` + base + `/v$V`,
		`case $(uname -m) in x86_64|amd64) A=amd64;; aarch64|arm64) A=arm64;; *) echo "vps-probe: no release for CPU $(uname -m)" >&2; exit 1;; esac`,
		`P=vps-probe-$V-linux-$A S=vps-probe-$V.sha256`,
		`D=$(mktemp -d)`,
		`trap "cd /; rm -rf $D" EXIT`,
		`cd "$D"`,
		// Give up on a host that does not answer (20s) or a transfer that
		// stalls (under 1 KB/s for 30s) rather than wait forever.
		`get() { if command -v curl >/dev/null 2>&1; then curl -fsSL --connect-timeout 20 --speed-limit 1024 --speed-time 30 --max-time 600 -o "$1" "$B/$1"; else wget -q -T 30 -t 2 -O "$1" "$B/$1"; fi || { echo "vps-probe: could not download $B/$1. If this machine cannot reach that host, use a mirror (-base URL) or copy the release here and run its install.sh." >&2; exit 1; }; }`,
		`get $P.tar.gz; get $S; get $S.sig`,
		`echo ` + b64([]byte(strings.Join(lines, "\n")+"\n")) + ` | base64 -d > signers`,
		`ssh-keygen -Y verify -f signers -I ` + principal + ` -n ` + Namespace + ` -s $S.sig < $S`,
		// The checksum line must exist: some sha256sum builds accept empty input.
		`grep " $P.tar.gz\$" $S > sum; test -s sum; sha256sum -c sum`,
		`tar xzf $P.tar.gz`,
		install,
	}, "; ")
	if strings.Contains(script, "'") {
		return "", errors.New("internal error: quote in the install script")
	}
	return " sh -c '" + script + "'", nil
}
