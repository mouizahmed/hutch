#!/usr/bin/env bash
# A disposable AWS rig for testing several machines: a lead and a worker with
# Firecracker (m8i, nested virtualization), and a worker with gVisor (AMD) whose
# security group admits only SSH, so it can only dial out. Stop it when not
# testing.
#
#   rig.sh up       create, or start, everything; SSH aliases rig-lead, rig-w1, rig-w2
#   rig.sh stop     stop the instances (their disks are kept)
#   rig.sh down     terminate the instances, delete the groups, empty and delete the bucket
#   rig.sh bucket   give the lead the rig's bucket (after hutch setup), and
#                   save its recovery details; rig.sh bucket --recovery, only those
#   rig.sh hutch …  the hutch CLI with the rig's own config; yours is untouched
#
# Needs the aws CLI with EC2 and S3 rights, RIG_KEY_NAME (an EC2 key pair) and
# RIG_KEY (its private key file). The bucket uses the CLI's own access keys.
set -euo pipefail
cd "$(dirname "$0")/../.."
: "${RIG_KEY_NAME:?an EC2 key pair name}" "${RIG_KEY:?its private key file}"
dir=${RIG_DIR:-$HOME/.cache/hutch-rig}
mkdir -p "$dir"
region=${AWS_REGION:-$(aws configure get region)}
ec2() { aws ec2 --region "$region" "$@"; }

declare -A type=([lead]=m8i.large [w1]=m8i.large [w2]=m8a.large)
declare -A nested=([lead]=enabled [w1]=enabled [w2]=)
declare -A group=([lead]=hutch-rig-lead [w1]=hutch-rig-lead [w2]=hutch-rig-closed)

instance() {
  ec2 describe-instances --filters "Name=tag:Name,Values=hutch-rig-$1" Name=instance-state-name,Values=pending,running,stopping,stopped \
    --query 'Reservations[].Instances[0].InstanceId' --output text
}

group_id() {
  local id
  id=$(ec2 describe-security-groups --filters "Name=group-name,Values=$1" --query 'SecurityGroups[0].GroupId' --output text)
  if [[ $id == None ]]; then
    id=$(ec2 create-security-group --group-name "$1" --description "hutch rig" --query GroupId --output text)
  fi
  echo "$id"
}

allow() { ec2 authorize-security-group-ingress --group-id "$1" --protocol tcp --port "$2" --cidr "$3" >/dev/null 2>&1 || true; }

up() {
  local me ami lead closed
  me=$(curl -fsS https://checkip.amazonaws.com)/32
  ami=$(ec2 describe-images --owners 099720109477 --filters 'Name=name,Values=ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*' \
    --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text)
  lead=$(group_id hutch-rig-lead)
  closed=$(group_id hutch-rig-closed)
  allow "$lead" 22 "$me"
  allow "$closed" 22 "$me"
  # The link is mutual TLS; workers dial it from wherever they are.
  allow "$lead" 7443 0.0.0.0/0
  for m in lead w1 w2; do
    local id
    id=$(instance "$m")
    if [[ -z $id || $id == None ]]; then
      local cpu=()
      [[ -n ${nested[$m]} ]] && cpu=(--cpu-options "NestedVirtualization=${nested[$m]}")
      id=$(ec2 run-instances --image-id "$ami" --instance-type "${type[$m]}" --key-name "$RIG_KEY_NAME" \
        --security-group-ids "$(group_id "${group[$m]}")" "${cpu[@]}" \
        --block-device-mappings 'DeviceName=/dev/sda1,Ebs={VolumeSize=60,VolumeType=gp3}' \
        --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=hutch-rig-$m}]" \
        --query 'Instances[0].InstanceId' --output text)
      echo "created hutch-rig-$m ($id, ${type[$m]})"
    else
      ec2 start-instances --instance-ids "$id" >/dev/null
    fi
  done
  ec2 wait instance-running --instance-ids $(for m in lead w1 w2; do instance "$m"; done)
  # Workers dial the address the lead had when they enrolled, so it keeps one.
  local eip
  eip=$(ec2 describe-addresses --filters Name=tag:Name,Values=hutch-rig-lead --query 'Addresses[0].AllocationId' --output text)
  if [[ $eip == None ]]; then
    eip=$(ec2 allocate-address --tag-specifications 'ResourceType=elastic-ip,Tags=[{Key=Name,Value=hutch-rig-lead}]' --query AllocationId --output text)
  fi
  ec2 associate-address --allocation-id "$eip" --instance-id "$(instance lead)" >/dev/null
  ssh_config
  if [[ ! -f $dir/bucket ]]; then
    local b
    b=hutch-rig-$(aws sts get-caller-identity --query Account --output text)-$(openssl rand -hex 3)
    aws s3api create-bucket --bucket "$b" --region "$region" >/dev/null
    echo "$b" >"$dir/bucket"
    echo "created bucket $b"
  fi
  for m in lead w1 w2; do
    until ssh -o ConnectTimeout=5 "rig-$m" true 2>/dev/null; do sleep 3; done
  done
  echo "rig is up: rig-lead, rig-w1, rig-w2"
}

ssh_config() {
  local cfg=$HOME/.ssh/config tmp
  tmp=$(mktemp)
  sed '/^# hutch-rig begin/,/^# hutch-rig end/d' "$cfg" >"$tmp" 2>/dev/null || true
  {
    echo "# hutch-rig begin"
    for m in lead w1 w2; do
      local ip
      ip=$(ec2 describe-instances --instance-ids "$(instance "$m")" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
      printf 'Host rig-%s\n  HostName %s\n  User ubuntu\n  IdentityFile %s\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n  LogLevel ERROR\n  ServerAliveInterval 30\n' "$m" "$ip" "$RIG_KEY"
    done
    echo "# hutch-rig end"
  } >>"$tmp"
  mv "$tmp" "$cfg"
  chmod 600 "$cfg"
}

ids() { for m in lead w1 w2; do id=$(instance "$m"); [[ -n $id && $id != None ]] && echo "$id"; done; }

case ${1:-} in
  up) up ;;
  stop) ids | xargs -r aws ec2 --region "$region" stop-instances --instance-ids >/dev/null && echo stopped ;;
  down)
    if [[ -n $(ids) ]]; then
      ec2 terminate-instances --instance-ids $(ids) >/dev/null
      ec2 wait instance-terminated --instance-ids $(ids)
    fi
    eip=$(ec2 describe-addresses --filters Name=tag:Name,Values=hutch-rig-lead --query 'Addresses[0].AllocationId' --output text)
    [[ $eip != None ]] && ec2 release-address --allocation-id "$eip"
    for g in hutch-rig-lead hutch-rig-closed; do
      id=$(ec2 describe-security-groups --filters "Name=group-name,Values=$g" --query 'SecurityGroups[0].GroupId' --output text)
      [[ $id != None ]] && ec2 delete-security-group --group-id "$id"
    done
    if [[ -f $dir/bucket ]]; then
      aws s3 rb "s3://$(cat "$dir/bucket")" --force >/dev/null && rm "$dir/bucket"
    fi
    sed -i '/^# hutch-rig begin/,/^# hutch-rig end/d' "$HOME/.ssh/config"
    rm -rf "$dir/config"
    echo "rig removed"
    ;;
  bucket)
    [[ -f $dir/passphrase ]] || openssl rand -hex 16 >"$dir/passphrase"
    jq -n --arg name "$(cat "$dir/bucket")" --arg region "$region" \
      --arg ak "$(aws configure get aws_access_key_id)" --arg sk "$(aws configure get aws_secret_access_key)" \
      --arg pass "$(cat "$dir/passphrase")" \
      '{bucket: {name: $name, region: $region, access_key: $ak, secret_key: $sk}, passphrase: $pass}' |
      XDG_CONFIG_HOME=$dir/config go run ./scripts/rig/bucket "${2:-rig-lead}"
    ;;
  hutch)
    shift
    XDG_CONFIG_HOME=$dir/config HUTCH_MACHINE=rig-lead exec bin/hutch "$@"
    ;;
  *) sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
