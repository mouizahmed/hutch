package aws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/mouizahmed/hutch/internal/provider"
)

// What hutch server init creates in an AWS account, besides workers.

// Account returns the account ID of the credentials.
func (a *AWS) Account(ctx context.Context) (string, error) {
	var out struct {
		Account string `xml:"GetCallerIdentityResult>Account"`
	}
	err := a.query(ctx, "sts", "sts."+a.cfg.Region+".amazonaws.com", "2011-06-15", map[string]string{"Action": "GetCallerIdentity"}, &out)
	return out.Account, err
}

// DefaultSubnet returns a subnet of the region's default VPC.
func (a *AWS) DefaultSubnet(ctx context.Context) (subnet, vpc string, err error) {
	var vpcs struct {
		IDs []string `xml:"vpcSet>item>vpcId"`
	}
	if err := a.ec2(ctx, "DescribeVpcs", map[string]string{"Filter.1.Name": "isDefault", "Filter.1.Value.1": "true"}, &vpcs); err != nil {
		return "", "", err
	}
	if len(vpcs.IDs) == 0 {
		return "", "", fmt.Errorf("no default VPC in %s", a.cfg.Region)
	}
	var subnets struct {
		Items []struct {
			ID    string `xml:"subnetId"`
			State string `xml:"state"`
		} `xml:"subnetSet>item"`
	}
	if err := a.ec2(ctx, "DescribeSubnets", map[string]string{"Filter.1.Name": "vpc-id", "Filter.1.Value.1": vpcs.IDs[0],
		"Filter.2.Name": "default-for-az", "Filter.2.Value.1": "true"}, &subnets); err != nil {
		return "", "", err
	}
	for _, s := range subnets.Items {
		if s.State == "available" {
			return s.ID, vpcs.IDs[0], nil
		}
	}
	return "", "", fmt.Errorf("no default subnet in %s", vpcs.IDs[0])
}

// ImportKey adds an SSH public key as a key pair, if it isn't there yet.
func (a *AWS) ImportKey(ctx context.Context, name string, public []byte) error {
	err := a.ec2(ctx, "ImportKeyPair", map[string]string{"KeyName": name, "PublicKeyMaterial": base64.StdEncoding.EncodeToString(public)}, nil)
	var e *apiError
	if errors.As(err, &e) && e.Code == "InvalidKeyPair.Duplicate" {
		return nil
	}
	return err
}

// CreateGroup makes the fleet's security group: SSH from one address (the CLI's),
// and port open between members (workers dialing the server).
func (a *AWS) CreateGroup(ctx context.Context, name, vpc, sshFrom, port string) (string, error) {
	p := map[string]string{"GroupName": name, "GroupDescription": "Hutch server and workers", "VpcId": vpc}
	tagParams(p, 1, "security-group", map[string]string{provider.TagFleet: a.cfg.Fleet})
	var out struct {
		ID string `xml:"groupId"`
	}
	if err := a.ec2(ctx, "CreateSecurityGroup", p, &out); err != nil {
		return "", err
	}
	err := a.ec2(ctx, "AuthorizeSecurityGroupIngress", map[string]string{
		"GroupId": out.ID, "IpPermissions.1.IpProtocol": "tcp", "IpPermissions.1.FromPort": "22", "IpPermissions.1.ToPort": "22",
		"IpPermissions.1.IpRanges.1.CidrIp": sshFrom + "/32", "IpPermissions.1.IpRanges.1.Description": "hutch CLI",
	}, nil)
	if err != nil {
		return out.ID, err
	}
	return out.ID, a.AllowWithinGroup(ctx, out.ID, port)
}

// boundary caps every role Hutch creates: whatever its own policy says, it can
// only reach Hutch's buckets.
const boundary = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject","s3:ListBucket"],"Resource":["arn:aws:s3:::hutch-*","arn:aws:s3:::hutch-*/*"]}]}`

func (a *AWS) iam(ctx context.Context, action string, params map[string]string, out any) error {
	params["Action"] = action
	err := a.query(ctx, "iam", "iam.amazonaws.com", "2010-05-08", params, out)
	var e *apiError
	if errors.As(err, &e) && e.Code == "EntityAlreadyExists" {
		return nil
	}
	return err
}

// CreateRole makes the role the server and workers reach the bucket with, as an
// instance profile of the same name. It is created with the hutch-boundary
// permissions boundary, which it creates first when it may.
func (a *AWS) CreateRole(ctx context.Context, account, name, bucket string) error {
	boundaryARN := "arn:aws:iam::" + account + ":policy/hutch-boundary"
	err := a.iam(ctx, "CreatePolicy", map[string]string{"PolicyName": "hutch-boundary", "PolicyDocument": boundary,
		"Description": "The most any Hutch role can do"}, nil)
	var e *apiError
	if err != nil && !(errors.As(err, &e) && e.Code == "AccessDenied") {
		return fmt.Errorf("boundary policy: %w", err)
	}
	trust := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	if err := a.iam(ctx, "CreateRole", map[string]string{"RoleName": name, "AssumeRolePolicyDocument": trust,
		"PermissionsBoundary": boundaryARN, "Description": "Hutch server and workers: the bucket"}, nil); err != nil {
		return fmt.Errorf("role: %w", err)
	}
	policy, _ := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": []map[string]any{
		{"Effect": "Allow", "Action": []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject"}, "Resource": "arn:aws:s3:::" + bucket + "/*"},
		{"Effect": "Allow", "Action": []string{"s3:ListBucket"}, "Resource": "arn:aws:s3:::" + bucket},
	}})
	if err := a.iam(ctx, "PutRolePolicy", map[string]string{"RoleName": name, "PolicyName": "bucket", "PolicyDocument": string(policy)}, nil); err != nil {
		return fmt.Errorf("role policy: %w", err)
	}
	if err := a.iam(ctx, "CreateInstanceProfile", map[string]string{"InstanceProfileName": name}, nil); err != nil {
		return fmt.Errorf("instance profile: %w", err)
	}
	err = a.iam(ctx, "AddRoleToInstanceProfile", map[string]string{"InstanceProfileName": name, "RoleName": name}, nil)
	if errors.As(err, &e) && e.Code == "LimitExceeded" { // already has it
		err = nil
	}
	return err
}

// UbuntuImage returns the newest Ubuntu 24.04 image for an architecture (amd64, arm64).
func (a *AWS) UbuntuImage(ctx context.Context, arch string) (string, error) {
	var out struct {
		Images []struct {
			ID      string    `xml:"imageId"`
			Created time.Time `xml:"creationDate"`
		} `xml:"imagesSet>item"`
	}
	if err := a.ec2(ctx, "DescribeImages", map[string]string{"Owner.1": "099720109477",
		"Filter.1.Name": "name", "Filter.1.Value.1": "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-" + arch + "-server-*",
		"Filter.2.Name": "state", "Filter.2.Value.1": "available"}, &out); err != nil {
		return "", err
	}
	best := ""
	var at time.Time
	for _, im := range out.Images {
		if im.Created.After(at) {
			best, at = im.ID, im.Created
		}
	}
	if best == "" {
		return "", fmt.Errorf("no Ubuntu 24.04 %s image in %s", arch, a.cfg.Region)
	}
	return best, nil
}

// ServerSpec is the always-on server instance.
type ServerSpec struct {
	Name, Type, Image, KeyName string
	DiskGB                     int
}

// LaunchServer starts the server instance, retrying while a new instance profile
// isn't visible to EC2 yet.
func (a *AWS) LaunchServer(ctx context.Context, spec ServerSpec) (*provider.Node, error) {
	tags := map[string]string{provider.TagFleet: a.cfg.Fleet, provider.TagRole: "server", "Name": spec.Name}
	p := map[string]string{
		"ImageId": spec.Image, "InstanceType": spec.Type, "MinCount": "1", "MaxCount": "1",
		"SubnetId": a.cfg.SubnetID, "SecurityGroupId.1": a.cfg.SecurityGroupID, "KeyName": spec.KeyName,
		"IamInstanceProfile.Name": a.cfg.InstanceProfile, "MetadataOptions.HttpTokens": "required",
		"ClientToken":                                  spec.Name + "-" + strconv.FormatInt(time.Now().Unix()/3600, 10),
		"BlockDeviceMapping.1.DeviceName":              "/dev/sda1",
		"BlockDeviceMapping.1.Ebs.VolumeSize":          strconv.Itoa(spec.DiskGB),
		"BlockDeviceMapping.1.Ebs.VolumeType":          "gp3",
		"BlockDeviceMapping.1.Ebs.Encrypted":           "true",
		"BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
	}
	tagParams(p, 1, "instance", tags)
	tagParams(p, 2, "volume", tags)
	for attempt := 0; ; attempt++ {
		var out struct {
			Instances []instance `xml:"instancesSet>item"`
		}
		err := a.ec2(ctx, "RunInstances", p, &out)
		var e *apiError
		if errors.As(err, &e) && e.Code == "InvalidParameterValue" && attempt < 20 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(out.Instances) != 1 {
			return nil, errors.New("aws: RunInstances returned no instance")
		}
		n := out.Instances[0].node()
		return &n, nil
	}
}

// Node returns one instance as it is now.
func (a *AWS) Node(ctx context.Context, id string) (*provider.Node, error) { return a.node(ctx, id) }
