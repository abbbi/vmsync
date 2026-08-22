#!/usr/bin/env bash
# Shared helpers for the vmsync benchmark harness (contrib/bench/bench.sh).
# Sourced, not meant to be run directly.

# --- logging -----------------------------------------------------------

# log/warn/die each do their timestamp + message in ONE printf builtin
# call ("%(FMT)T" is bash's own strftime, consuming the leading "-1" arg
# same as %s/%d would consume a string/int one) -- deliberately not a
# separate _ts() helper invoked via "$(...)": command substitution forks a
# subshell regardless of whether anything inside it execs an external
# process, and that fork is what actually dominates on platforms where
# process creation is heavy. Matters here specifically because the Stage 1
# matrix can log on the order of thousands of lines (hundreds of
# combinations x several log calls each) -- observed directly: with a
# separate _ts()+"$(...)" version of this, a --dry-run of the full default
# matrix took minutes on Windows/Git Bash from fork overhead alone, despite
# doing no real work at all. Requires bash >= 4.2 for "%(FMT)T", which
# every realistic target for this harness (a real Linux box running
# libvirt/vmsync) already has.
log()  { printf '[%(%Y-%m-%d %H:%M:%S)T] %s\n' -1 "$*"; }
warn() { printf '[%(%Y-%m-%d %H:%M:%S)T] WARNING: %s\n' -1 "$*" >&2; }
die()  { printf '[%(%Y-%m-%d %H:%M:%S)T] FATAL: %s\n' -1 "$*" >&2; exit 1; }

# --- timing --------------------------------------------------------------

now_epoch() { date +%s.%N; }

# elapsed_seconds START END -> prints the difference with 3 decimal places.
elapsed_seconds() {
	awk -v a="$1" -v b="$2" 'BEGIN { printf "%.3f", (b - a) }'
}

# --- ssh / virsh -----------------------------------------------------------

# ssh_host_cmd HOST CMD... - runs CMD on HOST as SSH_USER, using the same
# credentials configured for vmsync's own -ssh-* flags (bench.conf reuses
# one set of SSH settings for both vmsync itself and the harness's own
# direct ssh calls, e.g. qemu-io tampering). Only used for operations that
# aren't a libvirt API call -- see virsh_uri below for those.
ssh_host_cmd() {
	local host="$1"; shift
	local -a opts=(-o BatchMode=yes -o ConnectTimeout=10)
	[ -n "${SSH_KEY:-}" ] && opts+=(-i "$SSH_KEY")
	[ -n "${SSH_PORT:-}" ] && opts+=(-p "$SSH_PORT")
	[ -n "${SSH_KNOWN_HOSTS:-}" ] && opts+=(-o "UserKnownHostsFile=$SSH_KNOWN_HOSTS")
	ssh "${opts[@]}" "${SSH_USER}@${host}" "$@"
}

# maybe_ssh_cmd IS_LOCAL HOST CMD... - runs CMD directly on this machine
# when IS_LOCAL is "yes" (bench.sh is itself running on HOST), otherwise
# via ssh_host_cmd HOST CMD... as usual. Most of this harness's real work
# already goes over libvirt's own qemu(+ssh):// URI transport (SOURCE_URI/
# TARGET_URI), which is transport-agnostic on its own -- this only matters
# for the handful of call sites that shell out directly on a specific host
# (e.g. removing a leftover snapshot overlay file), which otherwise assume
# that host is always remote and reachable over SSH.
maybe_ssh_cmd() {
	local is_local="$1" host="$2"; shift 2
	if [ "$is_local" = yes ]; then
		"$@"
	else
		ssh_host_cmd "$host" "$@"
	fi
}

# virsh_uri URI ARGS... - runs virsh against a libvirt URI (source or
# target), reusing libvirt's own qemu+ssh:// transport instead of a second,
# separate ssh hop. Forces LC_ALL=C: virsh translates its own output via
# gettext (e.g. domstate prints "shut off"/"running" in English, but
# "fermé"/"en cours d'exécution" etc. under a French locale), and every
# exact-string comparison in this harness (dom_state's callers) assumes
# the untranslated English vocabulary -- this only affects virsh's own
# argv/output, not the operator's shell.
virsh_uri() {
	local uri="$1"; shift
	LC_ALL=C virsh -c "$uri" "$@"
}

# dom_state/domain_exists both leave virsh's own stderr text (if any) in
# $VIRSH_ERR on failure -- e.g. "-source-uri has the wrong scheme" and "the
# domain genuinely doesn't exist" and "SSH auth failed" all used to look
# identically like a bare "not found"/"cannot query state" with nothing
# else to go on. Callers that die/warn on failure should fold
# ${VIRSH_ERR:+: $VIRSH_ERR} into their own message.

# dom_state URI DOMAIN -> "running", "shut off", etc (whitespace collapsed).
dom_state() {
	local uri="$1" domain="$2"
	local out
	if out="$(virsh_uri "$uri" domstate "$domain" 2>&1)"; then
		VIRSH_ERR=""
		printf '%s' "$out" | tr -d '[:space:]'
		return 0
	fi
	VIRSH_ERR="$out"
	return 1
}

# domain_uuid URI DOMAIN -> the domain's UUID (whitespace collapsed), or
# empty + non-zero exit on failure (see dom_state's own VIRSH_ERR comment).
domain_uuid() {
	local uri="$1" domain="$2"
	local out
	if out="$(virsh_uri "$uri" domuuid "$domain" 2>&1)"; then
		VIRSH_ERR=""
		printf '%s' "$out" | tr -d '[:space:]'
		return 0
	fi
	VIRSH_ERR="$out"
	return 1
}

domain_exists() {
	local uri="$1" domain="$2"
	local out
	if out="$(virsh_uri "$uri" dominfo "$domain" 2>&1 >/dev/null)"; then
		VIRSH_ERR=""
		return 0
	fi
	VIRSH_ERR="$out"
	return 1
}

# virsh_err_is_not_found -- true if $VIRSH_ERR (as just set by
# domain_exists/dom_state) looks like libvirt's own "no such domain"
# error rather than some other failure (bad URI, auth, connection
# refused, ...). virsh ALWAYS writes something to stderr on any failure,
# so a bare "VIRSH_ERR is non-empty" check can never tell "doesn't exist"
# apart from a real connection problem -- this matches virsh's own two
# standard wordings for a missing domain ("failed to get domain 'X'" from
# dominfo/domstate's own CLI wrapper, "Domain not found: no domain with
# matching name/uuid 'X'" from libvirt's underlying error) so a genuine
# connection/auth failure still gets reported loudly instead of being
# silently treated as "just hasn't been synced yet".
virsh_err_is_not_found() {
	printf '%s' "$VIRSH_ERR" | grep -qiE "failed to get domain|domain not found|no domain with matching (name|uuid)"
}

require_dom_shutoff() {
	local uri="$1" domain="$2" label="$3"
	local state
	state="$(dom_state "$uri" "$domain")" || die "cannot query state of $label domain '$domain' via $uri${VIRSH_ERR:+: $VIRSH_ERR}"
	[ "$state" = "shutoff" ] || die "$label domain '$domain' is '$state', expected 'shut off' -- refusing to continue. This harness never starts the target VM itself; see README.md."
}

# require_dom_shutoff_or_absent: like require_dom_shutoff, but a target
# that doesn't exist at all is fine too -- DefineDomain (libvirtsync) is the
# only place that ever undefines/redefines the target, and it only does so
# once the full disk copy has already succeeded (-reinit itself deliberately
# leaves the target's own definition untouched; see cmd/vmsync/main.go's
# -reinit block for why), so a still-first-ever run, or one interrupted
# before that final step, legitimately leaves the target undefined, and the
# next full sync defines it from scratch. A genuine query failure (bad URI,
# auth, connection) is NOT the same thing and still dies loudly.
require_dom_shutoff_or_absent() {
	local uri="$1" domain="$2" label="$3"
	if domain_exists "$uri" "$domain"; then
		require_dom_shutoff "$uri" "$domain" "$label"
	elif ! virsh_err_is_not_found; then
		die "cannot query $label domain '$domain' via $uri: $VIRSH_ERR"
	fi
}

# require_dom_running: the external-snapshot lifecycle test (Stage 4) needs
# the SOURCE domain running -- removing an external disk-only snapshot via
# a live blockcommit/pivot is a running-domain operation, and that's also
# vmsync's own realistic use case (replicating a live VM). Unlike the
# target, this harness never controls the source's power state itself, so
# a source that isn't running just skips Stage 4 with a clear message
# rather than trying to start it.
require_dom_running() {
	local uri="$1" domain="$2" label="$3"
	local state
	state="$(dom_state "$uri" "$domain")" || die "cannot query state of $label domain '$domain' via $uri${VIRSH_ERR:+: $VIRSH_ERR}"
	[ "$state" = "running" ] || die "$label domain '$domain' is '$state', expected 'running' for the external-snapshot lifecycle test -- see README.md's Stage 4 section."
}

# disk_source_path URI DOMAIN DEV -> the <source file='...'/> path of
# DOMAIN's <target dev='DEV'> disk, per its own current domain XML. Uses
# xmllint (real XPath) rather than hand-rolled regex/awk XML parsing, which
# is why xmllint is a hard preflight requirement for this harness.
disk_source_path() {
	local uri="$1" domain="$2" dev="$3"
	virsh_uri "$uri" dumpxml "$domain" 2>/dev/null \
		| xmllint --xpath "string(//disk[target/@dev='${dev}']/source/@file)" - 2>/dev/null
}

# --- json (vmsync -read-fence) ------------------------------------------------

# json_str JSON FIELD -> the value of a string field, or empty.
# json_bool JSON FIELD -> "true"/"false", or empty when the field is absent.
#
# Deliberately grep, not jq: this harness's preflight requires vmsync, virsh,
# xmllint, awk and ssh, and adding a sixth hard dependency for two fields
# would be a poor trade. The only JSON it ever reads is vmsync's own
# -read-fence report, which is one flat object plus a nested "fence" whose
# key names ("id", "source") do not collide with the outer ones -- so a
# whole-document match per key is unambiguous. Anything more structured than
# that should get jq rather than a cleverer regex.
json_str() {
	printf '%s' "$1" | grep -o "\"$2\":\"[^\"]*\"" | head -1 | sed 's/^[^:]*:"//; s/"$//'
}

json_bool() {
	printf '%s' "$1" | grep -o "\"$2\":\(true\|false\)" | head -1 | sed 's/.*://'
}

# --- prometheus textfile parsing --------------------------------------------

# prom_sum FILE METRIC -> sum of every series' value for METRIC across all
# disks/labels in FILE (e.g. total bytes transferred across a multi-disk
# domain). Prints 0 if the file or metric is absent.
prom_sum() {
	local file="$1" metric="$2"
	[ -f "$file" ] || { printf '0\n'; return; }
	awk -v m="^${metric}{" '$0 ~ m { s += $NF } END { printf "%d\n", s+0 }' "$file"
}

# --- result recording --------------------------------------------------------

# results_init FILE - (re)creates the CSV results file with a header.
results_init() {
	printf 'scenario,phase,exit_code,wall_seconds,transferred_bytes,compressed_bytes,disk_bytes,mode,notes\n' > "$1"
}

# results_row FILE SCENARIO PHASE EXIT WALL_SECONDS TRANSFERRED COMPRESSED DISK MODE NOTES
# NOTES must never contain a literal comma -- this is a plain CSV, not a
# quoted/escaped one, kept deliberately simple since every caller in this
# harness controls its own note text.
results_row() {
	local file="$1"; shift
	printf '%s,%s,%s,%s,%s,%s,%s,%s,%s\n' "$@" >> "$file"
}
