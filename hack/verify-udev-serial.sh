#!/usr/bin/env bash
#
# Answer one question, on a real Oxide instance: can a udev rule match a disk by the NVMe serial
# Nexus derives from the Oxide disk name?
#
# Everything pkg/bootstrap/disk does rests on that. A generated rule gives each data disk a stable
# /dev/karpenter/<set>-<ordinal>, a static /etc/fstab keys its mounts on those names, and systemd
# does the waiting, the mounting and the ordering. If udev could not match on the serial, none of it
# would work and the node would have to resolve devices itself.
#
# Run it when bringing up a new node image, or when a node comes up without its data disks.
#
# Run it on a node Karpenter launched, with data disks attached:
#
#   sudo ./verify-udev-serial.sh
#
# It reads sysfs and, as root, installs one temporary udev rule file, triggers a re-probe, checks
# whether the symlinks appeared, and removes the rule again. The rule only adds symlinks: SYMLINK+=
# never renames or replaces a device node, so the boot disk is unaffected even though it is probed
# along with the rest. Without root the read-only checks still run and the live rule test is skipped.
#
# Exits nonzero if any check failed.

set -euo pipefail

# The probe's symlinks land in /dev/$PROBE_DIR/<n>. A directory of its own so the cleanup can be sure
# of what it is removing, and a name nothing else would pick.
readonly PROBE_DIR=karpenter-udev-probe
readonly RULE_FILE=/etc/udev/rules.d/99-karpenter-udev-probe.rules
# If this node was launched by this provider it already carries the table of disks it expects and the
# rules that name them, which turns the last section from "udev can match some serial" into "the
# names this node's fstab is keyed on are actually present".
readonly CONF="${KARPENTER_DISK_CONF:-/var/lib/karpenter/disks.conf}"
readonly RULES="${KARPENTER_DISK_RULES:-/etc/udev/rules.d/99-karpenter-disks.rules}"

passes=0
failures=0
warnings=0

ok() {
  printf '  ok    %s\n' "$*"
  passes=$((passes + 1))
}

bad() {
  printf '  FAIL  %s\n' "$*"
  failures=$((failures + 1))
}

warn() {
  printf '  warn  %s\n' "$*"
  warnings=$((warnings + 1))
}

info() { printf '        %s\n' "$*"; }

section() { printf '\n=== %s\n' "$*"; }

# Parallel arrays over the NVMe namespaces found, filled in by the sysfs section and read by the ones
# after it.
devices=()
serials=()
attr_paths=()

# ---------------------------------------------------------------- environment
section "environment"

info "$(uname -srm)"
info "bash ${BASH_VERSION}"

if command -v systemctl >/dev/null 2>&1; then
  systemd_version="$(systemctl --version | head -1)"
  info "$systemd_version"
  # x-systemd.requires= (systemd 233) and x-systemd.makefs (systemd 236) are what the declarative
  # half of the proposal would use. Anything shipping in a current node image is far past both, so
  # this is a sanity check rather than a real gate.
  version_number="$(printf '%s\n' "$systemd_version" | awk '{print $2}' | tr -cd '0-9')"
  if [ -n "$version_number" ] && [ "$version_number" -ge 236 ]; then
    ok "systemd $version_number supports x-systemd.requires= and x-systemd.makefs"
  else
    warn "could not confirm systemd is at least 236; x-systemd.makefs may be unavailable"
  fi
else
  bad "systemctl is not present, so none of this applies to this image"
fi

if command -v udevadm >/dev/null 2>&1; then
  ok "udevadm is present ($(udevadm --version 2>/dev/null || echo 'version unknown'))"
else
  bad "udevadm is not present, so udev rules are not an option on this image"
fi

for tool in blkid findmnt lsblk od; do
  command -v "$tool" >/dev/null 2>&1 || warn "$tool is missing; some output below will be thinner"
done

makefs=''
for candidate in /usr/lib/systemd/systemd-makefs /lib/systemd/systemd-makefs; do
  if [ -x "$candidate" ]; then
    makefs="$candidate"
    break
  fi
done
if [ -n "$makefs" ]; then
  info "systemd-makefs at $makefs (x-systemd.makefs would work, at the cost of mkfs options)"
else
  info "no systemd-makefs binary; x-systemd.makefs is not available, so mkfs stays in a script"
fi

# ------------------------------------------------------------ serials in sysfs
section "NVMe serials in sysfs"

# The boot disk is identified only so the output can label it. Nothing here treats it differently:
# the whole point of matching positively on a serial is that the boot disk needs no special case.
root_source="$(findmnt --noheadings --first-only -o SOURCE / 2>/dev/null || true)"
root_disk=''
if [ -n "$root_source" ]; then
  root_disk="$(lsblk --noheadings -o PKNAME "$root_source" 2>/dev/null | head -1 || true)"
  [ -n "$root_disk" ] || root_disk="$(basename "$root_source")"
fi

for namespace in /sys/block/nvme*n*; do
  [ -d "$namespace" ] || continue
  device="${namespace##*/}"
  # A partition is not a namespace. Rules should carry ENV{DEVTYPE}=="disk" for the same reason.
  if [ -e "$namespace/partition" ]; then
    continue
  fi

  serial=''
  attr_path=''
  # Both paths are tried because the attribute sits on the controller, which is one or two levels up
  # depending on the kernel. udev's ATTRS{} walks the whole chain, so it does not care which; the
  # sysfs read here does.
  for candidate in "$namespace/device/serial" "$namespace/device/device/serial"; do
    if [ -r "$candidate" ]; then
      serial="$(tr -d '\000' <"$candidate" | tr -d '[:space:]')"
      attr_path="$candidate"
      break
    fi
  done

  label="$device"
  if [ "$device" = "$root_disk" ]; then
    label="$device (boot disk)"
  fi

  if [ -z "$serial" ]; then
    bad "$label exposes no readable serial attribute in sysfs"
    continue
  fi

  ok "$label has serial '$serial'"
  info "  attribute: $attr_path"
  # Printed as bytes because how Nexus pads the 20-byte serial field is exactly the kind of thing
  # that decides whether a literal match in a udev rule works.
  if command -v od >/dev/null 2>&1; then
    info "  raw bytes: $(od -c <"$attr_path" | head -2 | tr '\n' ' ' | tr -s ' ')"
  fi
  info "  size:      $(( $(cat "$namespace/size" 2>/dev/null || echo 0) * 512 )) bytes"
  info "  fstype:    $(blkid -p -s TYPE -o value "/dev/$device" 2>/dev/null || echo '(none)')"

  # A serial carrying an fnmatch metacharacter would silently become a pattern in a rule rather than
  # a literal. Karpenter's names cannot, but a rule generator should refuse one that does.
  case "$serial" in
  *'*' | *'?' | *'['* | *'|'* | *'"'*)
    warn "  serial '$serial' contains an fnmatch metacharacter and could not be matched literally"
    ;;
  esac

  devices+=("$device")
  serials+=("$serial")
  attr_paths+=("$attr_path")
done

if [ "${#devices[@]}" -eq 0 ]; then
  bad "no NVMe namespaces with a serial were found; run this on a node with disks attached"
  printf '\n%s\n' "verification cannot continue"
  exit 1
fi

# ------------------------------------------------------ what udev itself sees
#
# This is the real question, and it is separate from the section above. sysfs having the attribute
# does not mean udev exposes it as ATTRS{serial} at a level a rule can match, and the value udev
# compares against is the one it prints here, not the one cat prints.
section "ATTRS{serial} as udev reports it"

if command -v udevadm >/dev/null 2>&1; then
  for i in "${!devices[@]}"; do
    device="${devices[i]}"
    walked="$(udevadm info --attribute-walk --name="/dev/$device" 2>/dev/null || true)"
    reported="$(
      printf '%s\n' "$walked" |
        sed -n 's/^[[:space:]]*ATTRS{serial}=="\(.*\)"[[:space:]]*$/\1/p' | head -1
    )"
    if [ -z "$reported" ]; then
      bad "$device: udevadm reports no ATTRS{serial} anywhere on its devpath"
      info "  a rule cannot match this disk by serial; the proposal does not apply"
      continue
    fi
    trimmed="${reported%"${reported##*[![:space:]]}"}"
    if [ "$trimmed" = "${serials[i]}" ]; then
      ok "$device: udev reports ATTRS{serial}==\"$reported\", matching sysfs after trimming"
    else
      bad "$device: udev reports ATTRS{serial}==\"$reported\" but sysfs holds '${serials[i]}'"
      info "  a rule would have to match udev's spelling, not the disk name's"
    fi
  done
else
  warn "udevadm is missing, skipping"
fi

# --------------------------------------------------------- the live rule test
section "live udev rule"

if [ "$(id -u)" -ne 0 ]; then
  warn "not running as root, skipping the live rule test (re-run with sudo for the real answer)"
else
  # Removed however this exits, including on a failed check under set -e, so a probe rule is never
  # left behind on a node.
  # shellcheck disable=SC2329  # invoked by the trap below
  cleanup() {
    rm -f "$RULE_FILE"
    udevadm control --reload >/dev/null 2>&1 || true
    udevadm trigger --subsystem-match=block --action=change >/dev/null 2>&1 || true
    udevadm settle --timeout=30 >/dev/null 2>&1 || true
    rmdir "/dev/$PROBE_DIR" 2>/dev/null || true
  }
  trap cleanup EXIT

  {
    printf '# Temporary, written by hack/verify-udev-serial.sh. Safe to delete.\n'
    for i in "${!devices[@]}"; do
      printf 'SUBSYSTEM=="block", ENV{DEVTYPE}=="disk", KERNEL=="nvme*n*", ATTRS{serial}=="%s", SYMLINK+="%s/%s"\n' \
        "${serials[i]}" "$PROBE_DIR" "$i"
    done
  } >"$RULE_FILE"
  info "wrote $RULE_FILE with ${#devices[@]} rule(s)"

  udevadm control --reload || bad "udevadm control --reload failed"
  udevadm trigger --subsystem-match=block --action=change || bad "udevadm trigger failed"
  udevadm settle --timeout=60 || warn "udevadm settle timed out; results below may be incomplete"

  for i in "${!devices[@]}"; do
    link="/dev/$PROBE_DIR/$i"
    device="${devices[i]}"
    if [ ! -e "$link" ]; then
      bad "$device: no symlink at $link, so the rule did not match"
      continue
    fi
    resolved="$(readlink -f "$link")"
    if [ "$resolved" = "/dev/$device" ]; then
      ok "$device: $link resolves to /dev/$device"
    else
      bad "$device: $link resolves to $resolved, which is a different disk"
      info "  matching by serial is not selecting the disk it names"
    fi
  done

  # The links have to survive a re-probe, since that is what a reboot amounts to for udev.
  udevadm trigger --subsystem-match=block --action=change >/dev/null 2>&1 || true
  udevadm settle --timeout=60 >/dev/null 2>&1 || true
  survived=0
  for i in "${!devices[@]}"; do
    if [ -e "/dev/$PROBE_DIR/$i" ]; then
      survived=$((survived + 1))
    fi
  done
  if [ "$survived" -eq "${#devices[@]}" ]; then
    ok "all ${#devices[@]} symlink(s) survived a second re-probe"
  else
    bad "only $survived of ${#devices[@]} symlink(s) survived a re-probe"
  fi
fi

# --------------------------------------------- against the disks this node has
#
# The sections above prove udev can match whatever serial a disk happens to present. This one proves
# the names this node's own fstab and raw block links are keyed on resolve to real disks, which is the
# claim that actually matters.
section "against $CONF"

if [ ! -r "$CONF" ]; then
  info "no disk table on this node, skipping (expected unless Karpenter launched it)"
else
  if [ -r "$RULES" ]; then
    info "$(grep -c 'SYMLINK' "$RULES" || true) rule(s) installed in $RULES"
  else
    warn "$RULES is missing, so nothing is naming this node's disks"
  fi
  while IFS=$'\t' read -r device filesystem mount link; do
    case "$device" in '' | '#'*) continue ;; esac
    if [ ! -e "$device" ]; then
      bad "$device does not exist, so its fstab entry cannot mount and its links dangle"
      continue
    fi
    resolved="$(readlink -f "$device")"
    ok "$device -> $resolved (filesystem: $filesystem)"
    if [ "$mount" != - ]; then
      if findmnt --noheadings --mountpoint "$mount" >/dev/null 2>&1; then
        info "  mounted at $mount"
      else
        bad "  $mount is not mounted"
      fi
    fi
    if [ "$link" != - ]; then
      if [ -e "$link" ]; then
        info "  linked at $link -> $(readlink -f "$link")"
      else
        bad "  the raw block link $link is missing"
      fi
    fi
  done <"$CONF"
fi

# ------------------------------------------------------------------- verdict
printf '\n=== verdict\n'
printf '  %d passed, %d failed, %d warning(s)\n\n' "$passes" "$failures" "$warnings"

if [ "$failures" -eq 0 ]; then
  cat <<'VERDICT'
  udev can match these disks by serial, which is what pkg/bootstrap/disk relies on: one generated
  rule per data disk gives it a stable /dev/karpenter/<set>-<ordinal>, /etc/fstab is keyed on those
  names at launch rather than rewritten per boot, and karpenter-disks.service is left with only the
  guarded mkfs, the seeding of a populated mount point and the SELinux relabel.
VERDICT
  exit 0
fi

cat <<'VERDICT'
  At least one check failed. Read the FAIL lines above before concluding anything: a missing tool or
  a node without data disks is not the same answer as udev being unable to match on the serial. If
  ATTRS{serial} is genuinely absent or matches the wrong disk on a new node image, that image cannot
  be used with diskSets: everything in pkg/bootstrap/disk keys on the names those rules create.
VERDICT
exit 1
