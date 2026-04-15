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

# Creates the EKS cluster and a Capacity Block neuron-efa node group.
# The capacity reservation must be created separately before running this script.
#
# The neuron-efa node group is created via CloudFormation (not eksctl or the
# EKS API directly) because:
#   1. eksctl does not pass the capacity reservation ID into the launch template
#   2. The EKS CreateNodegroup API does not attach EFA network interfaces from
#      the launch template for managed node groups
# CloudFormation handles both correctly.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Defaults (override via flags)
REGION="us-east-1"
CLUSTER_CONFIG="${SCRIPT_DIR}/cluster.yaml"
RESERVATION_ID=""
INSTANCE_TYPE="trn1.32xlarge"
INSTANCE_COUNT=2
PLACEMENT_GROUP="dranet-efa-demo-pg"

usage() {
  cat <<EOF
Usage: $0 [options]

Creates the EKS cluster and a Capacity Block neuron-efa node group.

Options:
  -i RESERVATION_ID   Capacity block reservation ID (default: auto-detect active reservation)
  -r REGION           AWS region (default: us-east-1)
  -h                  Show this help message

Prerequisites:
  - An active Capacity Block reservation for trn1.32xlarge instances
  - A placement group named "${PLACEMENT_GROUP}" in the target region
EOF
  exit 1
}

while getopts "i:r:h" opt; do
  case "$opt" in
    i) RESERVATION_ID="$OPTARG" ;;
    r) REGION="$OPTARG" ;;
    h) usage ;;
    *) usage ;;
  esac
done

# Auto-detect active capacity reservation if not provided
if [ -z "${RESERVATION_ID}" ]; then
  echo "==> No reservation ID provided, searching for active capacity reservations..."
  RESERVATION_ID=$(aws ec2 describe-capacity-reservations \
    --region "${REGION}" \
    --filters "Name=state,Values=active" "Name=instance-type,Values=${INSTANCE_TYPE}" \
    --query 'CapacityReservations[0].CapacityReservationId' \
    --output text)

  if [ -z "${RESERVATION_ID}" ] || [ "${RESERVATION_ID}" = "None" ]; then
    echo "Error: no active ${INSTANCE_TYPE} capacity reservation found in ${REGION}." >&2
    echo "       Create a capacity reservation first, or pass -i RESERVATION_ID." >&2
    exit 1
  fi
  echo "    Found: ${RESERVATION_ID}"
fi

# Verify the reservation is active
STATE=$(aws ec2 describe-capacity-reservations \
  --capacity-reservation-ids "${RESERVATION_ID}" \
  --region "${REGION}" \
  --query 'CapacityReservations[0].State' \
  --output text)

if [ "${STATE}" != "active" ]; then
  echo "Error: capacity reservation ${RESERVATION_ID} is in state '${STATE}', expected 'active'." >&2
  exit 1
fi

echo "==> Using capacity reservation: ${RESERVATION_ID} (state: ${STATE})"

# --- Step 1: Create EKS cluster with system node group ---
echo "==> Creating EKS cluster..."
eksctl create cluster -f "${CLUSTER_CONFIG}"

# Extract cluster name from the config
CLUSTER_NAME=$(grep -A1 '^metadata:' "${CLUSTER_CONFIG}" | grep 'name:' | awk '{print $2}')

# --- Step 2: Get cluster security group ---
echo "==> Looking up cluster security group..."
CLUSTER_SG=$(aws eks describe-cluster \
  --name "${CLUSTER_NAME}" \
  --region "${REGION}" \
  --query 'cluster.resourcesVpcConfig.clusterSecurityGroupId' \
  --output text)
echo "    Cluster security group: ${CLUSTER_SG}"

# --- Step 3: Get the node IAM role from the system node group ---
echo "==> Looking up node IAM role..."
NODE_ROLE=$(aws eks describe-nodegroup \
  --cluster-name "${CLUSTER_NAME}" \
  --nodegroup-name system \
  --region "${REGION}" \
  --query 'nodegroup.nodeRole' \
  --output text)
echo "    Node role: ${NODE_ROLE}"

# --- Step 4: Get the private subnet in the capacity reservation's AZ ---
echo "==> Looking up subnet..."
CR_AZ=$(aws ec2 describe-capacity-reservations \
  --capacity-reservation-ids "${RESERVATION_ID}" \
  --region "${REGION}" \
  --query 'CapacityReservations[0].AvailabilityZone' \
  --output text)

SUBNET_ID=$(aws ec2 describe-subnets \
  --region "${REGION}" \
  --filters \
    "Name=tag:alpha.eksctl.io/cluster-name,Values=${CLUSTER_NAME}" \
    "Name=availability-zone,Values=${CR_AZ}" \
    "Name=tag:Name,Values=*Private*" \
  --query 'Subnets[0].SubnetId' \
  --output text)
echo "    Subnet: ${SUBNET_ID} (${CR_AZ})"

# --- Step 5: Create neuron-efa node group via CloudFormation ---
# CloudFormation is required because:
# - The EKS CreateNodegroup API ignores NetworkInterfaces in launch templates
#   for managed node groups, so EFA interfaces are never attached
# - eksctl has a bug where it doesn't pass capacityReservationId to the
#   launch template
# CloudFormation handles both the launch template and the EKS nodegroup
# resource in a single stack, correctly attaching all 8 EFA interfaces.

STACK_NAME="${CLUSTER_NAME}-neuron-efa"
echo "==> Creating neuron-efa node group via CloudFormation: ${STACK_NAME}"

aws cloudformation create-stack \
  --stack-name "${STACK_NAME}" \
  --region "${REGION}" \
  --template-body "$(cat <<CFEOF
AWSTemplateFormatVersion: '2010-09-09'
Description: EKS managed nodegroup neuron-efa with EFA and Capacity Block

Resources:
  LaunchTemplate:
    Type: AWS::EC2::LaunchTemplate
    Properties:
      LaunchTemplateName: ${CLUSTER_NAME}-neuron-efa-lt
      LaunchTemplateData:
        InstanceType: ${INSTANCE_TYPE}
        InstanceMarketOptions:
          MarketType: capacity-block
        CapacityReservationSpecification:
          CapacityReservationTarget:
            CapacityReservationId: ${RESERVATION_ID}
        BlockDeviceMappings:
          - DeviceName: /dev/xvda
            Ebs:
              VolumeSize: 500
              VolumeType: gp3
              Iops: 3000
              Throughput: 125
        NetworkInterfaces:
          - DeviceIndex: 0
            InterfaceType: efa
            NetworkCardIndex: 0
            Groups:
              - ${CLUSTER_SG}
          - DeviceIndex: 1
            InterfaceType: efa
            NetworkCardIndex: 1
            Groups:
              - ${CLUSTER_SG}
          - DeviceIndex: 1
            InterfaceType: efa
            NetworkCardIndex: 2
            Groups:
              - ${CLUSTER_SG}
          - DeviceIndex: 1
            InterfaceType: efa
            NetworkCardIndex: 3
            Groups:
              - ${CLUSTER_SG}
          - DeviceIndex: 1
            InterfaceType: efa
            NetworkCardIndex: 4
            Groups:
              - ${CLUSTER_SG}
          - DeviceIndex: 1
            InterfaceType: efa
            NetworkCardIndex: 5
            Groups:
              - ${CLUSTER_SG}
          - DeviceIndex: 1
            InterfaceType: efa
            NetworkCardIndex: 6
            Groups:
              - ${CLUSTER_SG}
          - DeviceIndex: 1
            InterfaceType: efa
            NetworkCardIndex: 7
            Groups:
              - ${CLUSTER_SG}
        Placement:
          GroupName: ${PLACEMENT_GROUP}
        MetadataOptions:
          HttpTokens: required
          HttpPutResponseHopLimit: 2
        TagSpecifications:
          - ResourceType: instance
            Tags:
              - Key: Name
                Value: ${CLUSTER_NAME}-neuron-efa-Node

  ManagedNodeGroup:
    Type: AWS::EKS::Nodegroup
    DependsOn: LaunchTemplate
    Properties:
      ClusterName: ${CLUSTER_NAME}
      NodegroupName: neuron-efa
      NodeRole: ${NODE_ROLE}
      Subnets:
        - ${SUBNET_ID}
      CapacityType: CAPACITY_BLOCK
      AmiType: AL2023_x86_64_NEURON
      ScalingConfig:
        MinSize: ${INSTANCE_COUNT}
        MaxSize: ${INSTANCE_COUNT}
        DesiredSize: ${INSTANCE_COUNT}
      Labels:
        role: neuron-efa
      Taints:
        - Key: aws.amazon.com/neuron
          Effect: NO_SCHEDULE
          Value: ""
      LaunchTemplate:
        Id: !Ref LaunchTemplate
CFEOF
)" \
  --output text --query 'StackId'

echo "==> Waiting for CloudFormation stack to complete..."
aws cloudformation wait stack-create-complete \
  --stack-name "${STACK_NAME}" \
  --region "${REGION}"

echo "==> Waiting for neuron-efa node group to become active..."
aws eks wait nodegroup-active \
  --cluster-name "${CLUSTER_NAME}" \
  --nodegroup-name neuron-efa \
  --region "${REGION}"

echo "==> Done. Cluster is ready."
echo "    Cluster: ${CLUSTER_NAME}"
echo "    Capacity reservation: ${RESERVATION_ID}"
echo "    To clean up later, run: ./cleanup.sh"
