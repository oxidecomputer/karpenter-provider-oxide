#!/usr/bin/env bash
# Launches one Oxide instance per shape, records what the guest kernel keeps
# out of the shape's memory, and tears the instance down before the next one.
#
#   hack/guest-memory-survey.sh [ncpus-gib ...]
#
# Env: OXIDE_PROJECT (default brandon), IMAGE (boot image name), SSH_KEY
# (Oxide ssh key name), OUT (csv path). Shapes default to a 2x2 vCPU/memory
# factorial plus the thinnest-headroom ratio in the catalog.
set -euo pipefail

PROJECT="${OXIDE_PROJECT:-brandon}"
IMAGE="${IMAGE:-ubuntu-24-04}"
SSH_KEY="${SSH_KEY:-brandon}"
OUT="${OUT:-guest-memory-survey.csv}"
SHAPES=("$@")
(( ${#SHAPES[@]} )) || SHAPES=(2-8 16-8 2-64 96-64 10-5)

MiB=$((1 << 20)); GiB=$((1 << 30))
# Must match guestMemoryFixedBytes and guestMemoryLossBasisP in pkg/instancetype.
MODEL_FIXED_MIB=128; MODEL_BASISP=200

SSH_OPTS=(-o ConnectTimeout=5 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o LogLevel=ERROR)

log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" >&2; }

run_state() {
  local out
  out="$(oxide instance view --project "$PROJECT" --instance "$1" 2>/dev/null)" || return 0
  jq -r .run_state <<<"$out"
}

wait_state() {
  local name=$1 want=$2 i state
  for ((i = 0; i < 120; i++)); do
    state="$(run_state "$name")"
    [[ "$state" == "$want" ]] && return 0
    # A launch the rack cannot place lands in stopped or failed without ever running.
    [[ "$want" == running && ( "$state" == stopped || "$state" == failed ) ]] && return 1
    sleep 5
  done
  log "timed out waiting for $name to be $want"; return 1
}

teardown() {
  local name=$1 state
  state="$(run_state "$name")"
  [[ -z "$state" || "$state" == null ]] && return 0
  log "tearing down $name"
  if [[ "$state" != stopped ]]; then
    oxide instance stop --project "$PROJECT" --instance "$name" >/dev/null 2>&1 || true
    wait_state "$name" stopped || true
  fi
  oxide instance delete --project "$PROJECT" --instance "$name" >/dev/null
  oxide disk delete --project "$PROJECT" --disk "$name" >/dev/null 2>&1 || true
}

CURRENT=""
trap '[[ -n "$CURRENT" ]] && teardown "$CURRENT"' EXIT

launch() {
  local name=$1 ncpus=$2 gib=$3 body
  body="$(mktemp)"
  jq -n --arg name "$name" --arg image "$IMAGE_ID" --arg key "$SSH_KEY" \
    --argjson ncpus "$ncpus" --argjson mem "$((gib * GiB))" --argjson disk "$IMAGE_SIZE" '{
      name: $name, hostname: $name, description: "guest memory survey",
      ncpus: $ncpus, memory: $mem,
      boot_disk: {type: "create", name: $name, description: "guest memory survey", size: $disk,
                  disk_backend: {type: "distributed", disk_source: {type: "image", image_id: $image}}},
      network_interfaces: {type: "default_ipv4"},
      external_ips: [{type: "ephemeral"}],
      ssh_public_keys: [$key],
      auto_restart_policy: "never",
      start: true
    }' > "$body"
  local rc=0
  oxide instance create --project "$PROJECT" --json-body "$body" >/dev/null || rc=$?
  rm -f "$body"
  return "$rc"
}

# Prints key=value pairs for everything the guest kernel knows about its memory.
# shellcheck disable=SC2016 # expanded on the guest, not here
REMOTE='
set -e
phys=0
for d in /sys/firmware/memmap/*; do
  grep -qx "System RAM" "$d/type" || continue
  phys=$(( phys + $(cat "$d/end") - $(cat "$d/start") + 1 ))
done
echo "physical=$phys"
echo "nproc=$(nproc)"
echo "possible=$(cat /sys/devices/system/cpu/possible)"
echo "kernel=$(uname -r)"
awk "/^(MemTotal|MemAvailable|MemFree|Percpu|KernelStack|Slab|SUnreclaim|PageTables|HugePages_Total|Hugepagesize)/ {gsub(/:/,\"\",\$1); print \$1\"=\"\$2}" /proc/meminfo
line=$(sudo dmesg 2>/dev/null | grep -m1 "percpu: Embedded" || true)
echo "percpu_unit=$(echo "$line" | sed -n "s/.* u\([0-9]*\).*/\1/p")"
echo "percpu_pages=$(echo "$line" | sed -n "s/.*Embedded \([0-9]*\) pages.*/\1/p")"
'

collect() {
  local name=$1 i
  for ((i = 0; i < 90; i++)); do
    if ox ssh --project "$PROJECT" "${SSH_OPTS[@]}" "$name" -- "$REMOTE" 2>/dev/null; then return 0; fi
    sleep 10
  done
  log "could not ssh to $name"; return 1
}

IMAGE_JSON="$(oxide image view --project "$PROJECT" --image "$IMAGE")"
IMAGE_ID="$(jq -r .id <<<"$IMAGE_JSON")"
IMAGE_SIZE="$(jq -r .size <<<"$IMAGE_JSON")"

[[ -s "$OUT" ]] || echo "shape,ncpus,gib,kernel,physical_mib,memtotal_mib,loss_mib,loss_pct,model_mib,headroom_mib,percpu_unit_kib,percpu_total_mib,percpu_meminfo_mib,kernelstack_mib,slab_mib,pagetables_mib,idle_used_mib" > "$OUT"

for shape in "${SHAPES[@]}"; do
  ncpus="${shape%-*}"; gib="${shape#*-}"
  name="memsurvey-$shape"
  CURRENT="$name"
  teardown "$name"
  log "launching $shape"
  if ! launch "$name" "$ncpus" "$gib"; then
    log "launch of $shape refused, skipping"; CURRENT=""; continue
  fi
  if ! wait_state "$name" running; then
    log "$shape never reached running, skipping"; teardown "$name"; CURRENT=""; continue
  fi
  log "collecting from $shape"
  data="$(collect "$name")" || { teardown "$name"; CURRENT=""; continue; }
  teardown "$name"; CURRENT=""

  v() { sed -n "s/^$1=//p" <<<"$data" | head -1; }
  shape_bytes=$((gib * GiB))
  memtotal=$(( $(v MemTotal) * 1024 ))
  loss=$((shape_bytes - memtotal))
  model=$((MODEL_FIXED_MIB * MiB + shape_bytes * MODEL_BASISP / 10000))
  unit="$(v percpu_unit)"; unit="${unit:-0}"
  ppages="$(v percpu_pages)"
  awk -v shape="$shape" -v ncpus="$ncpus" -v gib="$gib" -v kernel="$(v kernel)" \
      -v phys="$(v physical)" -v memtotal="$memtotal" -v loss="$loss" -v model="$model" \
      -v shape_bytes="$shape_bytes" -v unit="$unit" -v ppages="${ppages:-0}" \
      -v percpu="$(v Percpu)" -v kstack="$(v KernelStack)" -v slab="$(v Slab)" \
      -v pt="$(v PageTables)" -v avail="$(v MemAvailable)" -v mt="$(v MemTotal)" '
    BEGIN { M = 2^20
      printf "%s,%d,%d,%s,%.0f,%.0f,%.0f,%.2f,%.0f,%.0f,%d,%.1f,%.1f,%.1f,%.1f,%.1f,%.0f\n",
        shape, ncpus, gib, kernel, phys/M, memtotal/M, loss/M, loss*100/shape_bytes,
        model/M, (model-loss)/M, unit/1024, ppages*4096*ncpus/M,
        percpu/1024, kstack/1024, slab/1024, pt/1024, (mt-avail)/1024 }' | tee -a "$OUT"
done

log "done; results in $OUT"
column -s, -t < "$OUT" >&2
