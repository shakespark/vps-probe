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

usage() {
	sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
}

# role -> names
set_role() {
	case "$1" in
	agent) NAME=vps-probe-agent USER_=vps-probe STATE=/var/lib/vps-probe CONF=$ETC/agent.yml CHECK="-check -config" VER=-version ;;
	server) NAME=vps-probe-server USER_=vps-probe-server STATE=/var/lib/vps-probe-server CONF=$ETC/server.yml CHECK="check -config" VER=version ;;
	echo) NAME=vps-probe-echo USER_=vps-probe-echo STATE=/var/lib/vps-probe-echo CONF=$ETC/echo.yml CHECK="-check -config" VER=-version ;;
	*) usage ;;
	esac
}

preflight() {
	[ "$(id -u)" = 0 ] || die "run as root"
	command -v systemctl >/dev/null 2>&1 || die "systemd is required"
	[ -x "$HERE/bin/$NAME" ] || die "$HERE/bin/$NAME not found; run this from the unpacked release directory"
	"$HERE/bin/$NAME" $VER >/dev/null 2>&1 ||
		die "$HERE/bin/$NAME does not run here: this package is for another CPU architecture? (this machine: $(uname -m))"
}

ensure_user() {
	if ! id -u "$USER_" >/dev/null 2>&1; then
		say "creating system user $USER_"
		useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$USER_"
	fi
}

# /etc/vps-probe is shared by agent and server: both service users must be
# able to enter it. The files themselves are 0640 root:<service group>.
ensure_etc() {
	install -d -m 0755 -o root -g root "$ETC"
	chown root:root "$ETC"
	chmod 0755 "$ETC"
}

# Validate as the service user, so permission problems show up now.
# $CHECK is two words (e.g. "-check -config") and must be split.
# shellcheck disable=SC2086
validate() {
	if command -v runuser >/dev/null 2>&1; then
		runuser -u "$USER_" -- "$BIN/$NAME" $CHECK "$1"
	else
		"$BIN/$NAME" $CHECK "$1"
	fi
}

install_binary() {
	say "installing $BIN/$NAME $("$HERE/bin/$NAME" $VER)"
	install -m 0755 -o root -g root "$HERE/bin/$NAME" "$BIN/$NAME.new"
	mv -f "$BIN/$NAME.new" "$BIN/$NAME"
}

# Returns 1 when there is no usable config yet (an example was installed).
install_config() {
	src=$1
	if [ -n "$src" ]; then
		[ -f "$src" ] || die "config $src not found"
		install -m 0640 -o root -g "$USER_" "$src" "$CONF.new"
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
		chown root:"$USER_" "$CONF"
		chmod 0640 "$CONF"
		validate "$CONF" || die "existing $CONF is not valid; fix it and run this again"
		say "keeping existing config $CONF"
		return 0
	fi
	example="$HERE/examples/$(basename "$CONF" .yml).example.yml"
	install -m 0640 -o root -g "$USER_" "$example" "$CONF"
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
	gid=$(id -g "$USER_")
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
	if [ "$NAME" = vps-probe-agent ]; then
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
	[ "$NAME" != vps-probe-agent ] || agent_icmp
	if ! install_config "$cfg"; then
		systemctl daemon-reload
		say "edit $CONF, then run: $0 $ROLE"
		exit 0
	fi
	start
	if [ "$NAME" = vps-probe-server ]; then
		cat <<-EOF

			Next steps (see README):
			  - allow UDP 9527 to this machine (cloud security group / firewall)
			  - per node: vps-probe-server add-node -id ID -server THIS_HOST:9527, restart
			    this service, then paste the command it prints on that VPS
			  - web UI: publish http://localhost:8080 through cloudflared + Access
		EOF
	fi
	if [ "$NAME" = vps-probe-echo ]; then
		cat <<-EOF

			Next steps (see README):
			  - allow the UDP port in $CONF to this machine (cloud security group / firewall)
			  - point the tunnel's far end at this port, and add an echo peer with the
			    same key to the probing node (server.yml extra_peers, then agent-config)
		EOF
	fi
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
		userdel "$USER_" 2>/dev/null || true
		rmdir "$ETC" 2>/dev/null || true # only if the other role isn't installed
	else
		say "kept $CONF and $STATE (use --purge to delete them)"
	fi
}

[ $# -ge 1 ] || usage
case "$1" in
agent | server | echo)
	ROLE=$1
	set_role "$ROLE"
	shift
	cmd_install "$@"
	;;
uninstall)
	[ $# -ge 2 ] || usage
	ROLE=$2
	set_role "$ROLE"
	shift 2
	cmd_uninstall "$@"
	;;
*) usage ;;
esac
