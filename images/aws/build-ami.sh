#!/usr/bin/env bash
# Build the worker image (AMI): Ubuntu 24.04 with hutchd, Firecracker, and the base
# guest image under /opt/hutch. A worker started from it formats its data disk and
# enrolls with the join token in its user data (hutchd boot); nothing is installed.
#
# usage: images/aws/build-ami.sh <hutchd> <dir> <version>
#   dir holds firecracker/<fc-version>/{firecracker,jailer} and images/base/{vmlinux,rootfs.ext4},
#   as on a worker's data disk
# env: AWS_REGION, HUTCH_KEY_NAME and HUTCH_KEY (an EC2 key pair and its private key),
#      HUTCH_SG (allows SSH from here), HUTCH_SUBNET
set -euo pipefail
hutchd=$1 dir=$2 version=$3
region=${AWS_REGION:-us-east-1}
ami=$(aws ec2 describe-images --region "$region" --owners 099720109477 \
  --filters 'Name=name,Values=ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*' Name=state,Values=available \
  --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text)
tags="{Key=Name,Value=hutch-ami-builder},{Key=hutch:build,Value=$version}"
id=$(aws ec2 run-instances --region "$region" --image-id "$ami" --instance-type c8i.large --key-name "$HUTCH_KEY_NAME" \
  --security-group-ids "$HUTCH_SG" --subnet-id "$HUTCH_SUBNET" --metadata-options HttpTokens=required \
  --block-device-mappings 'DeviceName=/dev/sda1,Ebs={VolumeSize=12,VolumeType=gp3,DeleteOnTermination=true}' \
  --tag-specifications "ResourceType=instance,Tags=[$tags]" --query 'Instances[0].InstanceId' --output text)
trap 'aws ec2 terminate-instances --region "$region" --instance-ids "$id" >/dev/null' EXIT
aws ec2 wait instance-running --region "$region" --instance-ids "$id"
ip=$(aws ec2 describe-instances --region "$region" --instance-ids "$id" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
S() { ssh -q -i "$HUTCH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 "ubuntu@$ip" "$@"; }
until S true 2>/dev/null; do sleep 3; done

S 'sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq xfsprogs >/dev/null && sudo mkdir -p /opt/hutch'
tar -C "$dir" -cSf - firecracker images | zstd -q -T0 | S 'zstd -q -d | sudo tar -C /opt/hutch -xSf -'
S 'sudo tee /usr/local/bin/hutchd >/dev/null && sudo chmod 755 /usr/local/bin/hutchd' < "$hutchd"
S 'sudo tee /etc/systemd/system/hutchd.service >/dev/null <<UNIT
[Unit]
Description=Hutch worker
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/hutchd boot
Restart=always
RestartSec=2
KillMode=process

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl enable hutchd && sudo ls -R /opt/hutch | head -20 && sudo cloud-init clean --logs'

image=$(aws ec2 create-image --region "$region" --instance-id "$id" --name "hutch-worker-$version-$(date +%Y%m%d%H%M%S)" \
  --tag-specifications "ResourceType=image,Tags=[{Key=hutch:worker-image,Value=$version}]" --query ImageId --output text)
aws ec2 wait image-available --region "$region" --image-ids "$image"
echo "$image"
