#!/bin/bash

# Copyright The Kubernetes Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Deletes the neuron-efa CloudFormation stack and the EKS cluster.
# Capacity Block reservations are managed separately.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Defaults
REGION="us-east-1"
CLUSTER_CONFIG="${SCRIPT_DIR}/cluster.yaml"

usage() {
  cat <<EOF
Usage: $0 [options]

Deletes the EKS cluster and neuron-efa node group. Capacity Block reservations are managed separately.

Options:
  -r REGION           AWS region (default: us-east-1)
  -h                  Show this help message
EOF
  exit 1
}

while getopts "r:h" opt; do
  case "$opt" in
    r) REGION="$OPTARG" ;;
    h) usage ;;
    *) usage ;;
  esac
done

CLUSTER_NAME=$(grep -A1 '^metadata:' "${CLUSTER_CONFIG}" | grep 'name:' | awk '{print $2}')
STACK_NAME="${CLUSTER_NAME}-neuron-efa"

echo "==> Deleting neuron-efa CloudFormation stack: ${STACK_NAME}"
aws cloudformation delete-stack \
  --stack-name "${STACK_NAME}" \
  --region "${REGION}" 2>/dev/null || echo "    Stack not found (already deleted or never created)"
aws cloudformation wait stack-delete-complete \
  --stack-name "${STACK_NAME}" \
  --region "${REGION}" 2>/dev/null || true

echo "==> Deleting EKS cluster..."
eksctl delete cluster -f "${CLUSTER_CONFIG}" --disable-nodegroup-eviction

echo "==> Cleanup complete."
echo "    Note: Capacity Block reservation was not cancelled. Manage it separately via the AWS console or CLI."
