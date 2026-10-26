// Package aws supplies workers from EC2: nested virtualization on 8th-generation
// Intel instances (c8i, m8i, r8i), no .metal.
package aws

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/mouizahmed/hutch/internal/provider"
	"github.com/mouizahmed/hutch/internal/sigv4"
)

// Families with nested virtualization.
var families = []string{"c8i", "m8i", "r8i"}

type Config struct {
	Region      string            `json:"region"`
	Credentials sigv4.Credentials `json:"credentials"`
	// Fleet tags everything this server creates, to tell it apart from anything else.
	Fleet           string `json:"fleet"`
	ImageID         string `json:"image_id"`
	SubnetID        string `json:"subnet_id"`
	SecurityGroupID string `json:"security_group_id"`
	InstanceProfile string `json:"instance_profile,omitempty"`
	KeyName         string `json:"key_name,omitempty"`
}

type AWS struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) (*AWS, error) {
	if cfg.Region == "" || cfg.Credentials.AccessKey == "" || cfg.Fleet == "" {
		return nil, errors.New("aws: region, credentials, and fleet are required")
	}
	return &AWS{cfg: cfg, client: &http.Client{Timeout: time.Minute}}, nil
}

func (a *AWS) Name() string { return "aws" }

// apiError is an AWS error response, query (XML) or JSON.
type apiError struct {
	Code, Message string
	Status        int
}

func (e *apiError) Error() string { return fmt.Sprintf("aws: %s: %s", e.Code, e.Message) }

// query calls a Query API (EC2, IAM) and decodes its XML response into out.
func (a *AWS) query(ctx context.Context, service, host, version string, params map[string]string, out any) error {
	q := map[string][]string{"Version": {version}}
	for k, v := range params {
		q[k] = []string{v}
	}
	body := []byte(sigv4.Query(q))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	region := a.cfg.Region
	if service == "iam" {
		region = "us-east-1"
	}
	sigv4.Sign(req, body, service, region, a.cfg.Credentials, time.Now())
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Code    string `xml:"Errors>Error>Code"`
			Message string `xml:"Errors>Error>Message"`
			// IAM
			ICode    string `xml:"Error>Code"`
			IMessage string `xml:"Error>Message"`
		}
		xml.Unmarshal(data, &e)
		if e.Code == "" {
			e.Code, e.Message = e.ICode, e.IMessage
		}
		if e.Code == "" {
			e.Code, e.Message = resp.Status, string(data)
		}
		return &apiError{Code: e.Code, Message: e.Message, Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	return xml.Unmarshal(data, out)
}

// json11 calls a JSON 1.1 API (Pricing, Cost Explorer).
func (a *AWS) json11(ctx context.Context, service, host, target string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", target)
	sigv4.Sign(req, body, service, "us-east-1", a.cfg.Credentials, time.Now())
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
			Msg     string `json:"Message"`
		}
		json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = e.Msg
		}
		return &apiError{Code: e.Type, Message: e.Message, Status: resp.StatusCode}
	}
	return json.Unmarshal(data, out)
}

func (a *AWS) ec2(ctx context.Context, action string, params map[string]string, out any) error {
	if params == nil {
		params = map[string]string{}
	}
	params["Action"] = action
	return a.query(ctx, "ec2", "ec2."+a.cfg.Region+".amazonaws.com", "2016-11-15", params, out)
}

type instance struct {
	ID        string    `xml:"instanceId"`
	Type      string    `xml:"instanceType"`
	State     string    `xml:"instanceState>name"`
	PrivateIP string    `xml:"privateIpAddress"`
	PublicIP  string    `xml:"ipAddress"`
	Image     string    `xml:"imageId"`
	Launch    time.Time `xml:"launchTime"`
	Tags      []struct {
		Key   string `xml:"key"`
		Value string `xml:"value"`
	} `xml:"tagSet>item"`
}

func (i instance) node() provider.Node {
	n := provider.Node{ID: i.ID, Type: i.Type, State: provider.NodeState(i.State), PrivateIP: i.PrivateIP, PublicIP: i.PublicIP, Image: i.Image, Launched: i.Launch, Tags: map[string]string{}}
	for _, t := range i.Tags {
		n.Tags[t.Key] = t.Value
		if t.Key == "Name" {
			n.Name = t.Value
		}
	}
	return n
}

func tagParams(p map[string]string, n int, resource string, tags map[string]string) {
	prefix := fmt.Sprintf("TagSpecification.%d.", n)
	p[prefix+"ResourceType"] = resource
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		p[fmt.Sprintf("%sTag.%d.Key", prefix, i+1)] = k
		p[fmt.Sprintf("%sTag.%d.Value", prefix, i+1)] = tags[k]
	}
}

func (a *AWS) ProvisionNode(ctx context.Context, spec provider.NodeSpec) (*provider.Node, error) {
	tags := map[string]string{provider.TagFleet: a.cfg.Fleet, provider.TagRole: "worker", "Name": spec.Name}
	for k, v := range spec.Tags {
		tags[k] = v
	}
	p := map[string]string{
		"ImageId":                                      a.cfg.ImageID,
		"InstanceType":                                 spec.Type,
		"MinCount":                                     "1",
		"MaxCount":                                     "1",
		"SubnetId":                                     a.cfg.SubnetID,
		"SecurityGroupId.1":                            a.cfg.SecurityGroupID,
		"UserData":                                     base64.StdEncoding.EncodeToString(spec.UserData),
		"CpuOptions.NestedVirtualization":              "enabled",
		"InstanceInitiatedShutdownBehavior":            "stop",
		"MetadataOptions.HttpTokens":                   "required",
		"ClientToken":                                  spec.Name,
		"BlockDeviceMapping.1.DeviceName":              "/dev/sdf",
		"BlockDeviceMapping.1.Ebs.VolumeSize":          strconv.Itoa(spec.DiskGB),
		"BlockDeviceMapping.1.Ebs.VolumeType":          "gp3",
		"BlockDeviceMapping.1.Ebs.Encrypted":           "true",
		"BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
	}
	if a.cfg.InstanceProfile != "" {
		p["IamInstanceProfile.Name"] = a.cfg.InstanceProfile
	}
	if a.cfg.KeyName != "" {
		p["KeyName"] = a.cfg.KeyName
	}
	tagParams(p, 1, "instance", tags)
	tagParams(p, 2, "volume", tags)
	var out struct {
		Instances []instance `xml:"instancesSet>item"`
	}
	if err := a.ec2(ctx, "RunInstances", p, &out); err != nil {
		return nil, err
	}
	if len(out.Instances) != 1 {
		return nil, errors.New("aws: RunInstances returned no instance")
	}
	n := out.Instances[0].node()
	return &n, nil
}

func (a *AWS) StopNode(ctx context.Context, id string) error {
	return a.ec2(ctx, "StopInstances", map[string]string{"InstanceId.1": id}, nil)
}

func (a *AWS) StartNode(ctx context.Context, id string) (*provider.Node, error) {
	if err := a.ec2(ctx, "StartInstances", map[string]string{"InstanceId.1": id}, nil); err != nil {
		return nil, err
	}
	return a.node(ctx, id)
}

func (a *AWS) DestroyNode(ctx context.Context, id string) error {
	return a.ec2(ctx, "TerminateInstances", map[string]string{"InstanceId.1": id}, nil)
}

func (a *AWS) node(ctx context.Context, id string) (*provider.Node, error) {
	nodes, err := a.describe(ctx, map[string]string{"InstanceId.1": id})
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("aws: no instance %s", id)
	}
	return &nodes[0], nil
}

func (a *AWS) ListNodes(ctx context.Context) ([]provider.Node, error) {
	return a.describe(ctx, map[string]string{
		"Filter.1.Name": "tag:" + provider.TagFleet, "Filter.1.Value.1": a.cfg.Fleet,
		"Filter.2.Name": "instance-state-name", "Filter.2.Value.1": "pending", "Filter.2.Value.2": "running",
		"Filter.2.Value.3": "stopping", "Filter.2.Value.4": "stopped",
	})
}

func (a *AWS) describe(ctx context.Context, params map[string]string) ([]provider.Node, error) {
	var nodes []provider.Node
	token := ""
	for {
		p := map[string]string{}
		for k, v := range params {
			p[k] = v
		}
		if token != "" {
			p["NextToken"] = token
		}
		var out struct {
			Reservations []struct {
				Instances []instance `xml:"instancesSet>item"`
			} `xml:"reservationSet>item"`
			NextToken string `xml:"nextToken"`
		}
		if err := a.ec2(ctx, "DescribeInstances", p, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Reservations {
			for _, i := range r.Instances {
				nodes = append(nodes, i.node())
			}
		}
		if out.NextToken == "" {
			return nodes, nil
		}
		token = out.NextToken
	}
}

func (a *AWS) InstanceTypes(ctx context.Context) ([]provider.InstanceType, error) {
	p := map[string]string{"Filter.1.Name": "instance-type"}
	for i, f := range families {
		p[fmt.Sprintf("Filter.1.Value.%d", i+1)] = f + ".*"
	}
	var types []provider.InstanceType
	token := ""
	for {
		q := map[string]string{}
		for k, v := range p {
			q[k] = v
		}
		if token != "" {
			q["NextToken"] = token
		}
		var out struct {
			Types []struct {
				Name   string   `xml:"instanceType"`
				VCPUs  int      `xml:"vCpuInfo>defaultVCpus"`
				MemMiB int      `xml:"memoryInfo>sizeInMiB"`
				Archs  []string `xml:"processorInfo>supportedArchitectures>item"`
				Maker  string   `xml:"processorInfo>manufacturer"`
				Metal  bool     `xml:"bareMetal"`
			} `xml:"instanceTypeSet>item"`
			NextToken string `xml:"nextToken"`
		}
		if err := a.ec2(ctx, "DescribeInstanceTypes", q, &out); err != nil {
			return nil, err
		}
		for _, t := range out.Types {
			if t.Metal || len(t.Archs) == 0 {
				continue
			}
			arch := map[string]string{"x86_64": "amd64", "arm64": "arm64"}[t.Archs[0]]
			types = append(types, provider.InstanceType{Name: t.Name, VCPUs: t.VCPUs, MemMiB: t.MemMiB, Arch: arch, Vendor: strings.ToLower(t.Maker)})
		}
		if out.NextToken == "" {
			break
		}
		token = out.NextToken
	}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i := range types {
		g.Go(func() error {
			price, err := a.price(gctx, "AmazonEC2", map[string]string{"instanceType": types[i].Name, "operatingSystem": "Linux",
				"tenancy": "Shared", "preInstalledSw": "NA", "capacitystatus": "Used"})
			if err != nil {
				return fmt.Errorf("price of %s: %w", types[i].Name, err)
			}
			mu.Lock()
			types[i].PerHour = price
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	sort.Slice(types, func(i, j int) bool {
		if types[i].VCPUs != types[j].VCPUs {
			return types[i].VCPUs < types[j].VCPUs
		}
		return types[i].Name < types[j].Name
	})
	return types, nil
}

// price returns the on-demand USD price of the one product in the region that
// matches the filters (the lowest tier, when it has several).
func (a *AWS) price(ctx context.Context, service string, match map[string]string) (float64, error) {
	type filter struct{ Type, Field, Value string }
	regionField := "regionCode"
	if service == "AWSDataTransfer" {
		regionField = "fromRegionCode"
	}
	filters := []filter{{"TERM_MATCH", regionField, a.cfg.Region}}
	for k, v := range match {
		filters = append(filters, filter{"TERM_MATCH", k, v})
	}
	var out struct {
		PriceList []string
	}
	if err := a.json11(ctx, "pricing", "api.pricing.us-east-1.amazonaws.com", "AWSPriceListService.GetProducts",
		map[string]any{"ServiceCode": service, "Filters": filters, "MaxResults": 10}, &out); err != nil {
		return 0, err
	}
	if len(out.PriceList) == 0 {
		return 0, fmt.Errorf("no %s price for %v", service, match)
	}
	var product struct {
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					BeginRange   string            `json:"beginRange"`
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}
	if err := json.Unmarshal([]byte(out.PriceList[0]), &product); err != nil {
		return 0, err
	}
	for _, term := range product.Terms.OnDemand {
		for _, d := range term.PriceDimensions {
			if d.BeginRange == "" || d.BeginRange == "0" {
				return strconv.ParseFloat(d.PricePerUnit["USD"], 64)
			}
		}
	}
	return 0, fmt.Errorf("no on-demand price for %v", match)
}

func (a *AWS) Rates(ctx context.Context) (provider.Rates, error) {
	var r provider.Rates
	lookups := []struct {
		dst     *float64
		service string
		match   map[string]string
	}{
		{&r.DiskGBMonth, "AmazonEC2", map[string]string{"productFamily": "Storage", "volumeApiName": "gp3"}},
		{&r.IPv4Hour, "AmazonVPC", map[string]string{"group": "VPCPublicIPv4Address", "groupDescription": "Hourly charge for In-use Public IPv4 Addresses"}},
		{&r.BucketGBMonth, "AmazonS3", map[string]string{"productFamily": "Storage", "volumeType": "Standard", "storageClass": "General Purpose"}},
		{&r.PutPer1000, "AmazonS3", map[string]string{"group": "S3-API-Tier1", "groupDescription": "PUT/COPY/POST or LIST requests"}},
		{&r.GetPer1000, "AmazonS3", map[string]string{"group": "S3-API-Tier2", "groupDescription": "GET and all other requests"}},
		{&r.EgressGB, "AWSDataTransfer", map[string]string{"transferType": "AWS Outbound", "toLocation": "External"}},
	}
	g, gctx := errgroup.WithContext(ctx)
	for _, l := range lookups {
		g.Go(func() error {
			v, err := a.price(gctx, l.service, l.match)
			if err != nil {
				return err
			}
			*l.dst = v
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return r, err
	}
	r.PutPer1000 *= 1000
	r.GetPer1000 *= 1000
	return r, nil
}

// BilledCosts reads Cost Explorer for one day, without credits and refunds, so the
// figure is what this fleet cost rather than what was charged after them.
func (a *AWS) BilledCosts(ctx context.Context, day time.Time) ([]provider.CostLine, error) {
	day = day.UTC().Truncate(24 * time.Hour)
	in := map[string]any{
		"TimePeriod":  map[string]string{"Start": day.Format(time.DateOnly), "End": day.AddDate(0, 0, 1).Format(time.DateOnly)},
		"Granularity": "DAILY",
		"Metrics":     []string{"UnblendedCost"},
		"Filter": map[string]any{"And": []any{
			map[string]any{"Tags": map[string]any{"Key": provider.TagFleet, "Values": []string{a.cfg.Fleet}}},
			map[string]any{"Not": map[string]any{"Dimensions": map[string]any{"Key": "RECORD_TYPE", "Values": []string{"Credit", "Refund"}}}},
		}},
		"GroupBy": []map[string]string{{"Type": "DIMENSION", "Key": "USAGE_TYPE"}, {"Type": "TAG", "Key": provider.TagWorker}},
	}
	var out struct {
		ResultsByTime []struct {
			Groups []struct {
				Keys    []string
				Metrics map[string]struct{ Amount string }
			}
		}
	}
	if err := a.json11(ctx, "ce", "ce.us-east-1.amazonaws.com", "AWSInsightsIndexService.GetCostAndUsage", in, &out); err != nil {
		return nil, err
	}
	var lines []provider.CostLine
	for _, r := range out.ResultsByTime {
		for _, g := range r.Groups {
			usd, _ := strconv.ParseFloat(g.Metrics["UnblendedCost"].Amount, 64)
			if usd == 0 || len(g.Keys) != 2 {
				continue
			}
			lines = append(lines, provider.CostLine{Day: day, Category: category(g.Keys[0]),
				Worker: strings.TrimPrefix(g.Keys[1], provider.TagWorker+"$"), USD: usd})
		}
	}
	return lines, nil
}

// category sorts an AWS usage type into the cost categories Hutch shows.
func category(usage string) string {
	switch {
	case strings.Contains(usage, "BoxUsage"):
		return "compute"
	case strings.Contains(usage, "EBS:"), strings.Contains(usage, "TimedStorage"), strings.Contains(usage, "Requests"):
		return "storage"
	case strings.Contains(usage, "DataTransfer"), strings.Contains(usage, "Bytes"):
		return "transfer"
	default:
		return "other"
	}
}

// WorkerImage finds the newest worker image (AMI) this account built, and the size
// of its root volume in GB.
func (a *AWS) WorkerImage(ctx context.Context) (string, int, error) {
	var out struct {
		Images []struct {
			ID      string    `xml:"imageId"`
			Created time.Time `xml:"creationDate"`
			State   string    `xml:"imageState"`
			Volumes []int     `xml:"blockDeviceMapping>item>ebs>volumeSize"`
		} `xml:"imagesSet>item"`
	}
	if err := a.ec2(ctx, "DescribeImages", map[string]string{"Owner.1": "self", "Filter.1.Name": "tag-key", "Filter.1.Value.1": "hutch:worker-image"}, &out); err != nil {
		return "", 0, err
	}
	best, size := "", 0
	var at time.Time
	for _, im := range out.Images {
		if im.State == "available" && im.Created.After(at) {
			best, at, size = im.ID, im.Created, 0
			for _, v := range im.Volumes {
				size += v
			}
		}
	}
	if best == "" {
		return "", 0, errors.New("no worker image in this account: build one with images/aws/build-ami.sh")
	}
	return best, size, nil
}

// AllowWithinGroup lets members of a security group reach a TCP port on each other:
// workers dialing the server.
func (a *AWS) AllowWithinGroup(ctx context.Context, group, port string) error {
	err := a.ec2(ctx, "AuthorizeSecurityGroupIngress", map[string]string{
		"GroupId": group, "IpPermissions.1.IpProtocol": "tcp", "IpPermissions.1.FromPort": port, "IpPermissions.1.ToPort": port,
		"IpPermissions.1.Groups.1.GroupId": group,
	}, nil)
	var e *apiError
	if errors.As(err, &e) && e.Code == "InvalidPermission.Duplicate" {
		return nil
	}
	return err
}

// Price returns an instance type's on-demand price per hour in the region.
func (a *AWS) Price(ctx context.Context, instanceType string) (float64, error) {
	return a.price(ctx, "AmazonEC2", map[string]string{"instanceType": instanceType, "operatingSystem": "Linux",
		"tenancy": "Shared", "preInstalledSw": "NA", "capacitystatus": "Used"})
}
