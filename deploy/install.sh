#!/bin/sh
# vps-probe installer. Run as root from the unpacked release directory.
#
#   ./install.sh agent  [--config FILE]     install or upgrade the agent
#   ./install.sh server [--config FILE]     install or upgrade the server
#   ./install.sh echo   [--config FILE]     install or upgrade the tunnel-probe
#                                           responder (only where a tunnel ends)
#   ./install.sh agent|server|echo --upgrade
#                                           upgrade only: fail if it is not
#                                           installed here yet
#   ./install.sh uninstall agent|server|echo [--purge]
#
# Safe to run again: upgrades replace the binary and unit and restart the
# service; an existing config is kept unless --config is given. Uninstall
# keeps config and data (including the agent's monthly traffic state)
# unless --purge is given.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
ETC=/etc/vps-probe
BIN=/usr/local/bin
UNITS=/etc/systemd/system

say() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# The usage text is this file's leading comment.
usage() {
	sed -n '2,/^set -eu/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
	exit 2
}

# Each role is a program vps-probe-<role>, run by a system user of the same
# name, with its config in $ETC/<role>.yml and its data in /var/lib/<name>.
set_role() {
	case "$1" in
	agent | server | echo) ;;
	*) usage ;;
	esac
	ROLE=$1
	NAME=vps-probe-$ROLE
	CONF=$ETC/$ROLE.yml
	STATE=/var/lib/$NAME
}

preflight() {
	[ "$(id -u)" = 0 ] || die "run as root"
	command -v systemctl >/dev/null 2>&1 || die "systemd is required"
	[ -f "$HERE/bin/$NAME" ] || die "$HERE/bin/$NAME not found; run this from the unpacked release directory"
	if ! "$HERE/bin/$NAME" version >/dev/null 2>&1; then
		if command -v findmnt >/dev/null 2>&1 && findmnt -no OPTIONS -T "$HERE" | grep -qw noexec; then
			die "$HERE is on a filesystem mounted noexec, so the programs in it cannot be run. Unpack the release somewhere else (for the one-line install command: put TMPDIR=/root in front of it)."
		fi
		die "$HERE/bin/$NAME does not run here: this package is for another CPU architecture? (this machine: $(uname -m))"
	fi
}

ensure_user() {
	if ! id -u "$NAME" >/dev/null 2>&1; then
		say "creating system user $NAME"
		useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$NAME"
	fi
}

# /etc/vps-probe is shared by the roles: every service user must be able to
# enter it. The files themselves are 0640 root:<service group>.
ensure_etc() {
	install -d -m 0755 -o root -g root "$ETC"
	chown root:root "$ETC"
	chmod 0755 "$ETC"
}

# Validate as the service user, so permission problems show up now.
validate() {
	if command -v runuser >/dev/null 2>&1; then
		runuser -u "$NAME" -- "$BIN/$NAME" check -config "$1"
	else
		"$BIN/$NAME" check -config "$1"
	fi
}

install_binary() {
	say "installing $BIN/$NAME $("$HERE/bin/$NAME" version)"
	install -m 0755 -o root -g root "$HERE/bin/$NAME" "$BIN/$NAME.new"
	mv -f "$BIN/$NAME.new" "$BIN/$NAME"
}

# Returns 1 when there is no usable config yet: the example was installed
# and needs to be filled in. (The server's example is usable as it is: a
# server without nodes.)
install_config() {
	src=$1
	if [ -n "$src" ]; then
		[ -f "$src" ] || die "config $src not found"
		install -m 0640 -o root -g "$NAME" "$src" "$CONF.new"
		if ! validate "$CONF.new"; then
			rm -f "$CONF.new"
			die "$src is not valid; nothing was changed"
		fi
		if [ -f "$CONF" ]; then
			bak="$CONF.bak-$(date +%Y%m%d-%H%M%S)"
			cp -p "$CONF" "$bak"
			say "previous config saved as $bak"
		fi
		mv -f "$CONF.new" "$CONF"
		say "installed config $CONF"
		return 0
	fi
	if [ -f "$CONF" ]; then
		chown root:"$NAME" "$CONF"
		chmod 0640 "$CONF"
		validate "$CONF" || die "existing $CONF is not valid; fix it and run this again"
		say "keeping existing config $CONF"
		return 0
	fi
	install -m 0640 -o root -g "$NAME" "$HERE/examples/$ROLE.example.yml" "$CONF"
	if validate "$CONF" >/dev/null 2>&1; then
		say "no config given: installed the example as $CONF"
		return 0
	fi
	warn "no config given: installed the example as $CONF"
	return 1
}

install_unit() {
	install -m 0644 -o root -g root "$HERE/systemd/$NAME.service" "$UNITS/$NAME.service"
}

# Unprivileged ICMP needs the service group inside net.ipv4.ping_group_range;
# otherwise grant CAP_NET_RAW to the agent service only (the host sysctl is
# left alone).
agent_icmp() {
	gid=$(id -g "$NAME")
	range=$(cat /proc/sys/net/ipv4/ping_group_range 2>/dev/null || echo "1 0")
	lo=${range%%[[:space:]]*}
	hi=${range##*[[:space:]]}
	dropin=$UNITS/$NAME.service.d/icmp.conf
	if [ "$gid" -ge "$lo" ] && [ "$gid" -le "$hi" ]; then
		return
	fi
	if [ ! -f "$dropin" ]; then
		say "ping_group_range ($range) excludes gid $gid: granting CAP_NET_RAW to $NAME via $dropin"
		install -d -m 0755 "$UNITS/$NAME.service.d"
		cat >"$dropin" <<-'EOF'
			[Service]
			# net.ipv4.ping_group_range excludes this user; allow raw ICMP instead.
			CapabilityBoundingSet=CAP_NET_RAW
			AmbientCapabilities=CAP_NET_RAW
		EOF
	fi
}

# In an LXC container lxcfs puts the container's own figures over
# /proc/meminfo, /proc/stat, /proc/uptime and others. ProtectProc,
# ProtectKernelTunables and ProtectControlGroups each give the service a new
# /proc without them, and the agent would report the host's memory and CPU.
# Binding the files back in (BindReadOnlyPaths) keeps the unit from starting
# at all once lxcfs has died, so these three are turned off instead. The agent
# stays an unprivileged user with no capabilities to write there.
agent_lxcfs() {
	dropin=$UNITS/$NAME.service.d/lxcfs.conf
	if ! grep -q ' fuse\.lxcfs ' /proc/mounts 2>/dev/null; then
		rm -f "$dropin"
		return
	fi
	[ -f "$dropin" ] || say "LXC container (lxcfs): keeping the container's /proc for $NAME via $dropin"
	install -d -m 0755 "$UNITS/$NAME.service.d"
	cat >"$dropin" <<-'EOF'
		[Service]
		# A new /proc would show the host's memory and CPU instead of this
		# container's (lxcfs).
		ProtectProc=default
		ProtectKernelTunables=no
		ProtectControlGroups=no
	EOF
}

start() {
	systemctl daemon-reload
	systemctl enable "$NAME" >/dev/null 2>&1
	since=$(date '+%Y-%m-%d %H:%M:%S')
	say "starting $NAME"
	systemctl restart "$NAME"
	sleep 3
	if ! systemctl is-active --quiet "$NAME"; then
		journalctl -u "$NAME" --since "$since" --no-pager -o cat | tail -n 20
		die "$NAME failed to start (log above)"
	fi
	if [ "$ROLE" = agent ]; then
		i=0
		while [ $i -lt 20 ]; do
			if journalctl -u "$NAME" --since "$since" --no-pager -o cat | grep -q 'server acknowledged'; then
				say "agent is reporting: the server acknowledged it"
				break
			fi
			i=$((i + 1))
			sleep 1
		done
		[ $i -lt 20 ] || warn "no acknowledgement from the server yet. Check that this node id and token are in server.yml, and that UDP reaches the server (see: journalctl -u $NAME)."
	fi
	journalctl -u "$NAME" --since "$since" --no-pager -o cat | grep -v '^$' | tail -n 8
}

cmd_install() {
	cfg=""
	upgrade=no
	while [ $# -gt 0 ]; do
		case "$1" in
		--config) [ $# -ge 2 ] || usage; cfg=$2; shift 2 ;;
		--upgrade) upgrade=yes; shift ;;
		*) usage ;;
		esac
	done
	[ $upgrade = no ] || [ -z "$cfg" ] || die "--upgrade keeps the existing config; it cannot be combined with --config"
	[ -z "$cfg" ] || cfg=$(cd "$(dirname "$cfg")" && pwd)/$(basename "$cfg")
	preflight
	# Without this an upgrade on the wrong machine would install the example
	# config and leave a service that reports nowhere.
	[ $upgrade = no ] || [ -f "$CONF" ] ||
		die "nothing to upgrade: $CONF does not exist. Install it first (for an agent: vps-probe-server install-cmd -node ID)."
	ensure_user
	ensure_etc
	install_binary
	install_unit
	if [ "$ROLE" = agent ]; then
		agent_icmp
		agent_lxcfs
	fi
	if ! install_config "$cfg"; then
		systemctl daemon-reload
		say "edit $CONF, then run: $0 $ROLE"
		exit 0
	fi
	start
	case $ROLE in
	server)
		cat <<-EOF

			Next steps (see README):
			  - allow UDP 9527 to this machine (cloud security group / firewall)
			  - set public_addr in $CONF: how agents reach this machine, HOST:9527
			  - per node: vps-probe-server add-node -id ID, restart this service, then
			    paste the command it prints on that VPS
			  - web UI: publish http://localhost:8080 through cloudflared + Access,
			    or an HTTPS reverse proxy with basic_auth
		EOF
		;;
	echo)
		cat <<-EOF

			Next steps (see docs/tunnels.md):
			  - allow the UDP port in $CONF to this machine (cloud security group / firewall)
			  - point the tunnel's far end at this port, and add an echo peer with the
			    same key to the probing node (its ping.extra in server.yml), then
			    reinstall that node's config
		EOF
		;;
	esac
}

cmd_uninstall() {
	purge=no
	[ "${1:-}" != --purge ] || purge=yes
	[ "$(id -u)" = 0 ] || die "run as root"
	say "stopping and removing $NAME"
	systemctl disable --now "$NAME" >/dev/null 2>&1 || true
	rm -f "$UNITS/$NAME.service" "$BIN/$NAME"
	rm -rf "$UNITS/$NAME.service.d"
	systemctl daemon-reload
	if [ $purge = yes ]; then
		say "purging $CONF and $STATE"
		rm -f "$CONF" "$CONF".bak-* "$CONF.new"
		rm -rf "$STATE"
		userdel "$NAME" 2>/dev/null || true
		rmdir "$ETC" 2>/dev/null || true # only if the other role isn't installed
	else
		say "kept $CONF and $STATE (use --purge to delete them)"
	fi
}

[ $# -ge 1 ] || usage
case "$1" in
agent | server | echo)
	set_role "$1"
	shift
	cmd_install "$@"
	;;
uninstall)
	[ $# -ge 2 ] || usage
	set_role "$2"
	shift 2
	cmd_uninstall "$@"
	;;
*) usage ;;
esac
