#!/usr/bin/env bash

set -euo pipefail

CONF="${KARPENTER_DISK_CONF:-/var/lib/karpenter/disks.conf}"
FSTAB_BLOCK="${KARPENTER_FSTAB_BLOCK:-/var/lib/karpenter/fstab.karpenter}"
FSTAB="${KARPENTER_FSTAB:-/etc/fstab}"
RUN_DIR="${KARPENTER_RUN_DIR:-/run/karpenter}"
DEVICE_WAIT_SECONDS="${KARPENTER_DEVICE_WAIT_SECONDS:-120}"
readonly CONF FSTAB_BLOCK FSTAB RUN_DIR DEVICE_WAIT_SECONDS

readonly ERRFILE="$RUN_DIR/disks.error"

readonly FSTAB_BEGIN='# BEGIN karpenter-managed mounts'
readonly FSTAB_END='# END karpenter-managed mounts'

log() {
  printf 'karpenter-disks: %s\n' "$*" >&2
  if [ -z "${JOURNAL_STREAM:-}" ] && command -v logger >/dev/null 2>&1; then
    logger -t karpenter-disks -- "$*" || true
  fi
}

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

# Bypass the udev cache after formatting.
probe_fstype() { blkid -p -s TYPE -o value "$1" 2>/dev/null || true; }

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

  log 'applying the udev rules that name this instance'"'"'s disks'
  udevadm control --reload || fail 'udevadm control --reload failed'
  udevadm trigger --subsystem-match=block --action=change ||
    fail 'udevadm trigger failed'
  udevadm settle --timeout=60 || log 'udevadm settle timed out, continuing'

  merge_fstab
  systemctl daemon-reload || fail 'systemctl daemon-reload failed'

  systemctl enable karpenter-disks.service || fail 'unable to enable karpenter-disks.service'
  systemctl start karpenter-disks.service || fail 'karpenter-disks.service failed; see journalctl -u karpenter-disks'

  local i unit
  for ((i = 0; i < n; i++)); do
    [ "${d_mount[i]}" != - ] || continue
    unit="$(systemd-escape --path --suffix=mount "${d_mount[i]}")" ||
      fail "unable to derive a mount unit name for ${d_mount[i]}"
    systemctl start "$unit" || fail "unable to mount ${d_mount[i]} (see systemctl status $unit)"
  done
  log 'disk setup installed'
}


wait_for_device() {
  local dev="$1" waited=0
  while [ ! -b "$dev" ]; do
    [ "$waited" -lt "$DEVICE_WAIT_SECONDS" ] || return 1
    sleep 1
    waited=$((waited + 1))
  done
}

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
  # Avoid root reserve, discard, and eager initialization on data disks.
  ext4)
    mkfs.ext4 -q -m 0 -E nodiscard,lazy_itable_init=1,lazy_journal_init=1 "$dev" ||
      fail "mkfs.ext4 failed on $dev"
    ;;
  xfs)
    mkfs.xfs -q -K "$dev" || fail "mkfs.xfs failed on $dev"
    ;;
  *)
    fail "$dev: unsupported filesystem '$want'"
    ;;
  esac
  FORMATTED=1
}

prepare_mount() {
  local dev="$1" fstype="$2" mp="$3" seed
  install -d -m 0755 "$mp" || fail "unable to create $mp"

  # Preserve existing mount-point contents.
  if [ -n "$(ls -A "$mp" 2>/dev/null)" ]; then
    seed="$(mktemp -d)" || fail "unable to create a staging mount point for $mp"
    mount -t "$fstype" "$dev" "$seed" || fail "unable to stage-mount $dev"
    log "seeding the new filesystem from the existing contents of $mp"
    cp -a "$mp/." "$seed/" || fail "unable to copy the existing contents of $mp"
    umount "$seed" || fail "unable to unmount the staging mount for $mp"
    rmdir "$seed" || true
  fi

  # Apply SELinux labels at the final mount path.
  if command -v selinuxenabled >/dev/null 2>&1 && selinuxenabled &&
    command -v restorecon >/dev/null 2>&1; then
    mount -t "$fstype" "$dev" "$mp" || fail "unable to mount $dev at $mp to relabel it"
    restorecon -R -F "$mp" || log "restorecon on $mp failed, continuing"
    umount "$mp" || fail "unable to unmount $mp after relabelling it"
  fi
}

do_boot() {
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

  if command -v udevadm >/dev/null 2>&1; then
    udevadm settle --timeout=60 || true
  fi

  for ((i = 0; i < n; i++)); do
    local dev="${d_dev[i]}"
    wait_for_device "$dev" ||
      fail "$dev never appeared; no attached disk presented the NVMe serial the udev rule matches on, so the disk is not attached to this instance"

    case "${d_fs[i]}" in
    none)
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
