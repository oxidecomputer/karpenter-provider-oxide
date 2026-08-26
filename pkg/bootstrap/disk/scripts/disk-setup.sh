#!/usr/bin/env bash
#
# Prepare the data disks a Karpenter launch attached to this instance.
#
# Installed at /var/lib/karpenter/disk-setup.sh by the node's user-data, run once at install time as
# "disk-setup.sh install", and re-run with no arguments on every boot by karpenter-disks.service.
# Nothing in here varies with the OxideNodeClass: everything that does is in the table at $CONF and
# in the udev rules and fstab block the user-data writes beside it.
#
# This deliberately does not resolve devices, mount anything, or order itself against the kubelet.
# udev names the disks and systemd mounts them:
#
#   * A generated udev rule matches each disk on the NVMe serial Nexus derives from the Oxide disk
#     name and gives it a stable /dev/karpenter/<set>-<ordinal>. Guest device numbering is not
#     deterministic, so every disk is matched positively on its serial and anything that does not
#     match is left strictly alone -- which is how the boot disk is excluded without ever being
#     identified. udev re-creates the symlinks on every boot, which is what makes the name usable in
#     a static /etc/fstab.
#
#   * Those fstab entries carry x-systemd.requires=karpenter-disks.service, so systemd runs this
#     script before it mounts anything, and RequiresMountsFor on the consumer's drop-in keeps the
#     kubelet down if a mount fails.
#
# What is left is the three things neither udev nor systemd will do: format a blank disk the way we
# want it formatted, seed a mount point whose contents would otherwise be hidden, and relabel a fresh
# filesystem for SELinux. x-systemd.makefs covers the first but takes no mkfs options and offers no
# hook for the other two, which is why this script still exists.
#
# The table is tab separated, one line per data disk:
#
#   device <TAB> filesystem <TAB> mount <TAB> link
#
# device is the /dev/karpenter path udev creates. filesystem is ext4 or xfs for a disk this script
# formats, none for a raw block device that is only symlinked, or - for a disk created from an image
# or a snapshot, which arrives already carrying one. mount and link are absolute paths, or - where
# they do not apply. No field may contain whitespace, a quote, a backslash or a newline: the
# controller refuses a nodeclass that would produce one.

set -euo pipefail

# Overridable only so the script can be exercised off a node. A node sets none of these.
CONF="${KARPENTER_DISK_CONF:-/var/lib/karpenter/disks.conf}"
FSTAB_BLOCK="${KARPENTER_FSTAB_BLOCK:-/var/lib/karpenter/fstab.karpenter}"
FSTAB="${KARPENTER_FSTAB:-/etc/fstab}"
RUN_DIR="${KARPENTER_RUN_DIR:-/run/karpenter}"
# How long to wait for a disk that is attached but whose udev symlink has not appeared yet. A disk
# that never appears is a failure, not something to wait out: the launch attached it, so its absence
# means something is wrong rather than slow.
DEVICE_WAIT_SECONDS="${KARPENTER_DEVICE_WAIT_SECONDS:-120}"
readonly CONF FSTAB_BLOCK FSTAB RUN_DIR DEVICE_WAIT_SECONDS

# ErrorPath in pkg/bootstrap/disk is the same value; a test keeps the two in step.
readonly ERRFILE="$RUN_DIR/disks.error"

# The block of $FSTAB the user-data owns. Delimited so it can be replaced wholesale rather than
# appended to, which is what makes installing over a node that already has one idempotent.
readonly FSTAB_BEGIN='# BEGIN karpenter-managed mounts'
readonly FSTAB_END='# END karpenter-managed mounts'

# Writes to stderr, and additionally to syslog only when stderr is not already the journal.
#
# systemd sets JOURNAL_STREAM when it has connected stdout and stderr to the journal, which it has
# here: the unit sets StandardError=journal. Calling logger as well would put every line in twice and
# fork a process to do it. The install step is the case this is for -- it runs from user-data under
# cloud-init, where stderr lands in cloud-init-output.log rather than the journal.
log() {
  printf 'karpenter-disks: %s\n' "$*" >&2
  if [ -z "${JOURNAL_STREAM:-}" ] && command -v logger >/dev/null 2>&1; then
    logger -t karpenter-disks -- "$*" || true
  fi
}

# Every exit that is not success goes through here: the reason lands in the journal and in a file for
# whoever gets a shell on the node.
fail() {
  install -d -m 0755 "$RUN_DIR" 2>/dev/null || true
  printf '%s\n' "$*" >"$ERRFILE" 2>/dev/null || true
  log "FATAL: $*"
  exit 1
}

require() {
  command -v "$1" >/dev/null 2>&1 ||
    fail "required tool '$1' is not in PATH; the node image must provide it (package: $2)"
}

# blkid -p probes the device itself rather than the udev cache, which is what makes this correct
# immediately after a mkfs in the same run.
probe_fstype() { blkid -p -s TYPE -o value "$1" 2>/dev/null || true; }

# ------------------------------------------------------------------- the table
d_dev=() d_fs=() d_mount=() d_link=()
n=0

read_table() {
  [ -r "$CONF" ] ||
    fail "$CONF is missing or unreadable; the node's user-data should have written it"
  local f_dev f_fs f_mount f_link
  while IFS=$'\t' read -r f_dev f_fs f_mount f_link; do
    case "$f_dev" in '' | '#'*) continue ;; esac
    d_dev[n]="$f_dev"
    d_fs[n]="$f_fs"
    d_mount[n]="$f_mount"
    d_link[n]="$f_link"
    n=$((n + 1))
  done <"$CONF"
  [ "$n" -gt 0 ] ||
    fail "$CONF describes no disks, so this unit should never have been installed"
}

# ---------------------------------------------------------------- install time
#
# Everything here happens once, from the user-data, before the node joins the cluster. It is in this
# script rather than rendered into the user-data so that all of the shell a node runs is one linted,
# reviewable file.
merge_fstab() {
  [ -r "$FSTAB_BLOCK" ] || fail "$FSTAB_BLOCK is missing; the user-data should have written it"
  local merged
  merged="$(mktemp "$FSTAB.karpenter.XXXXXX")" ||
    fail "unable to create a temporary file beside $FSTAB; is /etc read-only?"
  awk -v b="$FSTAB_BEGIN" -v e="$FSTAB_END" '
    $0 == b { skip = 1; next }
    $0 == e { skip = 0; next }
    !skip   { print }
  ' "$FSTAB" >"$merged"
  {
    printf '%s\n' "$FSTAB_BEGIN"
    cat "$FSTAB_BLOCK"
    printf '%s\n' "$FSTAB_END"
  } >>"$merged"
  chmod 0644 "$merged"
  mv -f "$merged" "$FSTAB" || fail "unable to replace $FSTAB"
}

do_install() {
  require udevadm systemd
  require systemctl systemd
  require systemd-escape systemd
  require awk awk
  require mktemp coreutils

  # The rules file is already on disk. Reloading and re-triggering is what turns it into symlinks now
  # rather than at the next boot, and settle is what makes the rest of this synchronous.
  log 'applying the udev rules that name this instance'"'"'s disks'
  udevadm control --reload || fail 'udevadm control --reload failed'
  udevadm trigger --subsystem-match=block --action=change ||
    fail 'udevadm trigger failed'
  udevadm settle --timeout=60 || log 'udevadm settle timed out, continuing'

  merge_fstab
  # Mount units are generated from /etc/fstab by systemd-fstab-generator, which only runs on a
  # daemon-reload, so this is what turns the block above into units at all.
  systemctl daemon-reload || fail 'systemctl daemon-reload failed'

  systemctl enable karpenter-disks.service || fail 'unable to enable karpenter-disks.service'
  # Synchronous on purpose. systemctl start on a Type=oneshot unit blocks until ExecStart finishes
  # and exits nonzero if it failed, so under set -e nothing below runs on a node whose disks are not
  # ready to be mounted.
  systemctl start karpenter-disks.service || fail 'karpenter-disks.service failed; see journalctl -u karpenter-disks'

  local i unit
  for ((i = 0; i < n; i++)); do
    [ "${d_mount[i]}" != - ] || continue
    unit="$(systemd-escape --path --suffix=mount "${d_mount[i]}")" ||
      fail "unable to derive a mount unit name for ${d_mount[i]}"
    # Started through systemd rather than with mount(8) so that the unit is active from the first
    # boot and the consumer's RequiresMountsFor has something to depend on.
    systemctl start "$unit" || fail "unable to mount ${d_mount[i]} (see systemctl status $unit)"
  done
  log 'disk setup installed'
}

# ------------------------------------------------------------------- boot time

# Waits for udev to publish the symlink naming one disk. -b follows the link, so this is true only
# once the link exists and points at a block device.
wait_for_device() {
  local dev="$1" waited=0
  while [ ! -b "$dev" ]; do
    [ "$waited" -lt "$DEVICE_WAIT_SECONDS" ] || return 1
    sleep 1
    waited=$((waited + 1))
  done
}

# ensure_fs <device> <filesystem>; sets FORMATTED to 0 or 1.
#
# The guard is the point. A device that already carries a filesystem is never reformatted, which is
# also what makes an image- or snapshot-sourced disk safe to hand to this script. A device carrying
# the wrong filesystem is a hard failure rather than a reformat, because the only thing that can tell
# "wrong" from "somebody's data" is a human. mkfs is invoked without -f or -F so that it refuses on
# its own as an independent second guard.
ensure_fs() {
  local dev="$1" want="$2" have
  have="$(probe_fstype "$dev")"
  if [ -n "$have" ]; then
    [ "$have" = "$want" ] ||
      fail "$dev already carries a '$have' filesystem but the nodeclass asks for '$want'; refusing to reformat a device that holds data"
    FORMATTED=0
    log "$dev already has $have, leaving it alone"
    return 0
  fi
  log "formatting $dev as $want"
  case "$want" in
  # -m 0 because a data volume needs no root reserve. nodiscard and lazy init because a full discard
  # and inode-table write of a large Crucible-backed disk delays registration for no gain. These are
  # the options x-systemd.makefs cannot express, and the reason this script still formats.
  ext4)
    mkfs.ext4 -q -m 0 -E nodiscard,lazy_itable_init=1,lazy_journal_init=1 "$dev" ||
      fail "mkfs.ext4 failed on $dev"
    ;;
  # -K is the xfs equivalent of nodiscard.
  xfs)
    mkfs.xfs -q -K "$dev" || fail "mkfs.xfs failed on $dev"
    ;;
  *)
    fail "$dev: unsupported filesystem '$want'"
    ;;
  esac
  FORMATTED=1
}

# prepare_mount <device> <filesystem> <mountpoint>
#
# The two things that are impossible once systemd has mounted the filesystem, done here on the one
# boot that formatted it. Both use a transient mount of their own, because systemd owns the real one.
prepare_mount() {
  local dev="$1" fstype="$2" mp="$3" seed
  install -d -m 0755 "$mp" || fail "unable to create $mp"

  # A mount over a populated directory hides whatever the image put there, which for a path like
  # /var/lib/rancher means hiding the agent's own preinstalled binaries and image tarballs and
  # failing later in a way that is hard to trace back to here.
  if [ -n "$(ls -A "$mp" 2>/dev/null)" ]; then
    seed="$(mktemp -d)" || fail "unable to create a staging mount point for $mp"
    mount -t "$fstype" "$dev" "$seed" || fail "unable to stage-mount $dev"
    log "seeding the new filesystem from the existing contents of $mp"
    cp -a "$mp/." "$seed/" || fail "unable to copy the existing contents of $mp"
    umount "$seed" || fail "unable to unmount the staging mount for $mp"
    rmdir "$seed" || true
  fi

  # A freshly created filesystem carries no SELinux labels, which breaks container runtimes on
  # RHEL-family images in ways that are very hard to diagnose from the kubelet's side. restorecon
  # derives a context from the path, so the filesystem has to be at its real path while it runs --
  # a staging directory would label it as whatever lives under /tmp.
  if command -v selinuxenabled >/dev/null 2>&1 && selinuxenabled &&
    command -v restorecon >/dev/null 2>&1; then
    mount -t "$fstype" "$dev" "$mp" || fail "unable to mount $dev at $mp to relabel it"
    restorecon -R -F "$mp" || log "restorecon on $mp failed, continuing"
    umount "$mp" || fail "unable to unmount $mp after relabelling it"
  fi
}

do_boot() {
  # Only what this particular layout uses is required, and all of it is checked before any device is
  # touched. Without that, a layout with three ext4 sets and one xfs set formats three disks and then
  # dies, leaving a half-configured machine for somebody to reason about.
  require blkid util-linux
  require mount util-linux
  require umount util-linux
  require mktemp coreutils
  local i
  for ((i = 0; i < n; i++)); do
    case "${d_fs[i]}" in
    ext4) require mkfs.ext4 e2fsprogs ;;
    xfs) require mkfs.xfs xfsprogs ;;
    esac
  done

  install -d -m 0755 "$RUN_DIR"
  rm -f "$ERRFILE"

  command -v udevadm >/dev/null 2>&1 && udevadm settle --timeout=60 || true

  for ((i = 0; i < n; i++)); do
    local dev="${d_dev[i]}"
    wait_for_device "$dev" ||
      fail "$dev never appeared; no attached disk presented the NVMe serial the udev rule matches on, so the disk is not attached to this instance"

    case "${d_fs[i]}" in
    none)
      # A raw block device: the consumer owns its contents, so nothing is probed and nothing is
      # formatted. The link is remade every boot rather than once because it is cheap and because it
      # is the only thing that repairs a node somebody has poked at.
      install -d -m 0755 "$(dirname "${d_link[i]}")" ||
        fail "unable to create the directory holding ${d_link[i]}"
      ln -sfn "$dev" "${d_link[i]}" || fail "unable to create the link ${d_link[i]}"
      ;;
    ext4 | xfs)
      ensure_fs "$dev" "${d_fs[i]}"
      if [ "$FORMATTED" = 1 ] && [ "${d_mount[i]}" != - ]; then
        prepare_mount "$dev" "${d_fs[i]}" "${d_mount[i]}"
      fi
      ;;
    '-')
      # Created from an image or a snapshot, so it arrives carrying a filesystem. An empty one means
      # the source was not what the nodeclass said it was, which is a failure rather than an
      # invitation to format it.
      [ -n "$(probe_fstype "$dev")" ] ||
        fail "$dev carries no filesystem, but it was created from an image or a snapshot and is never formatted"
      ;;
    *)
      fail "$dev: unsupported filesystem '${d_fs[i]}'"
      ;;
    esac
  done

  log 'all disks ready to mount'
}

read_table
case "${1:-boot}" in
install) do_install ;;
boot) do_boot ;;
*) fail "usage: ${0##*/} [install|boot]" ;;
esac
