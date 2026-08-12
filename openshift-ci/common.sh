#!/usr/bin/bash
set -euo pipefail

pushd ../../
git log -1 || true # just printing commit in the test output
# we move to content of https://github.com/openshift-metal3/dev-scripts.git repo
# we need to change folder as {common,network}.sh have source files
# shellcheck source=network.sh #https://github.com/koalaman/shellcheck/wiki/SC1090
source ./common.sh
# shellcheck source=network.sh
source ./network.sh
popd

wait_for_pods() {
  local namespace=$1
  local selector=$2

  echo "waiting for pods $namespace - $selector to be created"
  timeout 5m bash -c "until [[ -n \$(oc get pods -n $namespace -l $selector 2>/dev/null) ]]; do sleep 5; done"
  echo "waiting for pods $namespace to be ready"
  oc wait --for=condition=Ready --all pods -n "$namespace" --timeout=5m
  echo "pods for $namespace with selector $selector are ready"
}

enable_frr_k8s_debug() {
  local FRRK8S_NAMESPACE="openshift-frr-k8s"
  echo "Enabling debug for frr-k8s"
  oc create ns ${FRRK8S_NAMESPACE} || true

  oc apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: env-overrides
  namespace: ${FRRK8S_NAMESPACE}
data:
  frrk8s-loglevel: "--log-level=debug"
  frrk8s-poll-interval: "--poll-interval=5s"
EOF
}

wait_for_csv() {
  local namespace=$1
  local csv=$2

  timeout 5m bash -c "until oc get csv -n $namespace $csv; do sleep 5; done"
  oc wait --for jsonpath='{.status.phase}'=Succeeded csv/"$csv" -n "$namespace" --timeout=300s
}

nthhost_pureshell() {
  local network="$1"
  local idx="$2"

  if [[ -z "$network" || -z "$idx" ]]; then
    echo "Usage: nthhost_pureshell <subnet_cidr> <offset>" >&2
    return 1
  fi

  if [[ "$network" == *":"* ]]; then
    # --- IPv6 Logic (Pure shell parsing + bc) ---
    local ip="${network%%/*}"
    local prefix="${network##*/}"

    # Expand '::' shorthand natively
    if [[ "$ip" == *"::"* ]]; then
      local left="${ip%%::*}"
      local right="${ip#*::}"

      local left_groups=0 right_groups=0 missing fill="" i
      [[ -n "$left" ]] && left_groups=$(( $(grep -o ":" <<< "$left" | wc -l) + 1 ))
      [[ -n "$right" ]] && right_groups=$(( $(grep -o ":" <<< "$right" | wc -l) + 1 ))

      missing=$(( 8 - left_groups - right_groups ))
      for ((i=0; i<missing; i++)); do
        [[ -z "$fill" ]] && fill="0" || fill="${fill}:0"
      done

      if [[ -n "$left" && -n "$right" ]]; then
        ip="${left}:${fill}:${right}"
      elif [[ -n "$left" ]]; then
        ip="${left}:${fill}"
      elif [[ -n "$right" ]]; then
        ip="${fill}:${right}"
      else
        ip="${fill}"
      fi
    fi

    # Convert groups into 32 hex chars
    local groups hex="" g
    IFS=':' read -r -a groups <<< "$ip"
    for g in "${groups[@]}"; do
      hex="${hex}$(printf "%04x" "0x${g:-0}")"
    done

    # Calculate target integer and format back to IPv6
    local hex_upper dec_target hex_target hex_padded
    hex_upper=$(echo "$hex" | tr 'a-f' 'A-F')

    dec_target=$(bc <<< "
      ibase=16
      net=$hex_upper
      host_bits=128 - $prefix
      net_base = (net / 2^host_bits) * 2^host_bits
      net_base + $idx
    ")

    hex_target=$(bc <<< "obase=16; $dec_target")
    hex_padded=$(printf "%032s" "$hex_target" | tr ' ' '0')
    
    echo "$hex_padded" | sed -E 's/(.{4})/\1:/g' | sed 's/:$//' | tr 'A-F' 'a-f'

  else
    # --- IPv4 Logic (Native bitwise math) ---
    local ip="${network%%/*}"
    local prefix="${network##*/}"
    local a b c d ip_int mask net_int target_int
    
    IFS='.' read -r a b c d <<< "$ip"
    ip_int=$(( (a << 24) + (b << 16) + (c << 8) + d ))
    
    mask=$(( (0xFFFFFFFF << (32 - prefix)) & 0xFFFFFFFF ))
    net_int=$(( ip_int & mask ))
    target_int=$(( net_int + idx ))
    
    echo "$(( (target_int >> 24) & 255 )).$(( (target_int >> 16) & 255 )).$(( (target_int >> 8) & 255 )).$(( target_int & 255 ))"
  fi
}
