package asset

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nezhahq/nezha/model"
)

const aliyun = "aliyun"

type aliyunProvider struct {
	http    *http.Client
	account cloudAccount
}

type cloudBill struct {
	InstanceID, Currency string
	Amount, Gross        json.Number
}

type provider interface {
	verify(context.Context) error
	inventory(context.Context, string) (map[string]model.AssetCloud, error)
	bills(context.Context, string) ([]cloudBill, error)
}

type aliyunReply struct {
	Code    string
	Success *bool
}

func rpcEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func rpcSignature(query url.Values, secret string) string {
	canonical := strings.ReplaceAll(query.Encode(), "+", "%20")
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	_, _ = io.WriteString(mac, "GET&%2F&"+rpcEscape(canonical))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p aliyunProvider) call(ctx context.Context, endpoint, version, action string, query url.Values, out any) error {
	query = cloneQuery(query)
	query.Set("Action", action)
	query.Set("Version", version)
	query.Set("Format", "JSON")
	query.Set("AccessKeyId", p.account.AccessKeyID)
	query.Set("SignatureMethod", "HMAC-SHA1")
	query.Set("SignatureVersion", "1.0")
	if p.account.SecurityToken != "" {
		query.Set("SecurityToken", p.account.SecurityToken)
	}
	var failure error
	// Cap request rate, and retry provider throttling, transient HTTP and transport errors.
	for attempt := 0; attempt < 4; attempt++ {
		if err := pause(ctx, 220*time.Millisecond+time.Duration(attempt*attempt)*time.Second); err != nil {
			return err
		}
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return errors.New("could not create request nonce")
		}
		query.Set("SignatureNonce", hex.EncodeToString(nonce))
		query.Set("Timestamp", time.Now().UTC().Format(time.RFC3339))
		query.Del("Signature")
		query.Set("Signature", rpcSignature(query, p.account.AccessKeySecret))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+endpoint+"/?"+query.Encode(), nil)
		if err != nil {
			return fmt.Errorf("%s: invalid request", action)
		}
		resp, err := p.http.Do(req)
		if err != nil {
			failure = fmt.Errorf("%s: connection failed", action)
			continue
		}
		const maxBody = 32 << 20
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if err != nil {
			failure = fmt.Errorf("%s: response read failed", action)
			continue
		}
		if len(body) > maxBody {
			return fmt.Errorf("%s: response too large", action)
		}
		var reply aliyunReply
		validJSON := json.Unmarshal(body, &reply) == nil
		throttled := resp.StatusCode == http.StatusTooManyRequests || strings.HasPrefix(reply.Code, "Throttling") || reply.Code == "TooManyRequests"
		if throttled || resp.StatusCode >= 500 {
			failure = fmt.Errorf("%s: temporarily unavailable (HTTP %d)", action, resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK || (reply.Success != nil && !*reply.Success) || (reply.Code != "" && !strings.EqualFold(reply.Code, "Success")) {
			reason := "provider rejected the request"
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				reason = "check the account credentials and RAM policy"
			}
			return fmt.Errorf("%s: %s (HTTP %d)", action, reason, resp.StatusCode)
		}
		if !validJSON || json.Unmarshal(body, out) != nil {
			return fmt.Errorf("%s: invalid response", action)
		}
		return nil
	}
	return failure
}

func cloneQuery(values url.Values) url.Values {
	copy := make(url.Values, len(values))
	for k, v := range values {
		copy[k] = slices.Clone(v)
	}
	return copy
}

func (p aliyunProvider) verify(ctx context.Context) error {
	var caller struct {
		AccountID    string
		IdentityType string
	}
	if err := p.call(ctx, "sts.aliyuncs.com", "2015-04-01", "GetCallerIdentity", nil, &caller); err != nil {
		return err
	}
	if caller.AccountID != p.account.Account || (caller.IdentityType != "RAMUser" && caller.IdentityType != "AssumedRoleUser") {
		return errors.New("STS caller must be a RAM user or assumed role in the configured account")
	}
	return nil
}

type aliyunInstance struct {
	InstanceID, InstanceName, InstanceType, RegionID, ZoneID, Status, InstanceChargeType string
	OSType, OSName, OSNameEn, CreationTime, ExpiredTime, InternetChargeType              string
	CPU                                                                                  int
	Memory                                                                               int64
	InternetMaxBandwidthOut                                                              *int
	PublicIPAddress, InnerIPAddress                                                      struct{ IPAddress []string }
	VPCAttributes                                                                        struct{ PrivateIPAddress struct{ IPAddress []string } }
	EIPAddress                                                                           struct {
		IPAddress, InternetChargeType string
		Bandwidth                     *int
	}
	NetworkInterfaces struct {
		NetworkInterface []struct {
			PrimaryIPAddress string
			PrivateIPSets    struct {
				PrivateIPSet []struct{ PrivateIPAddress string }
			}
			IPv6Sets struct {
				IPv6Set []struct{ IPv6Address string }
			}
		}
	}
}

func (i aliyunInstance) snapshot() model.AssetCloud {
	public := slices.Clone(i.PublicIPAddress.IPAddress)
	private := append(slices.Clone(i.InnerIPAddress.IPAddress), i.VPCAttributes.PrivateIPAddress.IPAddress...)
	if i.EIPAddress.IPAddress != "" {
		public = append(public, i.EIPAddress.IPAddress)
	}
	for _, nic := range i.NetworkInterfaces.NetworkInterface {
		if nic.PrimaryIPAddress != "" {
			private = append(private, nic.PrimaryIPAddress)
		}
		for _, address := range nic.PrivateIPSets.PrivateIPSet {
			if address.PrivateIPAddress != "" {
				private = append(private, address.PrivateIPAddress)
			}
		}
		for _, address := range nic.IPv6Sets.IPv6Set {
			if address.IPv6Address != "" {
				private = append(private, address.IPv6Address)
			}
		}
	}
	slices.Sort(public)
	slices.Sort(private)
	plan := model.AssetPlan{PublicIPs: strings.Join(slices.Compact(public), ", "), PrivateIPs: strings.Join(slices.Compact(private), ", "), CPUCores: i.CPU, MemoryMiB: i.Memory, OS: i.OSNameEn}
	if plan.OS == "" {
		plan.OS = i.OSName
	}
	if plan.OS == "" {
		plan.OS = i.OSType
	}
	bandwidth := i.InternetMaxBandwidthOut
	if i.EIPAddress.Bandwidth != nil {
		bandwidth = i.EIPAddress.Bandwidth
	}
	if bandwidth != nil {
		plan.Bandwidth = strconv.Itoa(*bandwidth) + " Mbps"
	}
	out := model.AssetCloud{AssetPlan: plan, Name: i.InstanceName, Zone: i.ZoneID, InstanceType: i.InstanceType, Status: i.Status, PurchasedAt: ParseDate(i.CreationTime), Billing: "unknown"}
	switch i.InstanceChargeType {
	case "PrePaid":
		out.Billing, out.ExpiresAt = "subscription", ParseDate(i.ExpiredTime)
	case "PostPaid":
		out.Billing = "on_demand"
	}
	return out
}

func paginate(nextPage func(string) (string, error)) error {
	seen := make(map[string]bool)
	token := ""
	for {
		next, err := nextPage(token)
		if err != nil || next == "" {
			return err
		}
		if seen[next] {
			return errors.New("provider repeated a pagination token")
		}
		seen[next], token = true, next
	}
}

func (p aliyunProvider) inventory(ctx context.Context, region string) (map[string]model.AssetCloud, error) {
	endpoint := "ecs." + region + ".aliyuncs.com"
	instances := make(map[string]model.AssetCloud)
	var subscriptions []string
	err := paginate(func(token string) (string, error) {
		var page struct {
			NextToken string
			Instances *struct{ Instance []aliyunInstance }
		}
		query := url.Values{"RegionId": {region}, "MaxResults": {"100"}}
		if token != "" {
			query.Set("NextToken", token)
		}
		if err := p.call(ctx, endpoint, "2014-05-26", "DescribeInstances", query, &page); err != nil {
			return "", err
		}
		if page.Instances == nil {
			return "", errors.New("DescribeInstances: missing instance collection")
		}
		for _, item := range page.Instances.Instance {
			if item.InstanceID == "" || (item.RegionID != "" && item.RegionID != region) {
				return "", errors.New("DescribeInstances: invalid instance identity")
			}
			snapshot := item.snapshot()
			if _, exists := instances[item.InstanceID]; !exists && snapshot.Billing == "subscription" {
				subscriptions = append(subscriptions, item.InstanceID)
			}
			instances[item.InstanceID] = snapshot
		}
		return page.NextToken, nil
	})
	if err != nil || len(instances) == 0 {
		return instances, err
	}

	err = paginate(func(token string) (string, error) {
		var page struct {
			NextToken string
			Disks     *struct {
				Disk []struct {
					DiskID, InstanceID string
					Size               int64
					Attachments        struct{ Attachment []struct{ InstanceID string } }
					MountInstance      []struct{ InstanceID string }
				}
			}
		}
		query := url.Values{"RegionId": {region}, "MaxResults": {"100"}}
		if token != "" {
			query.Set("NextToken", token)
		}
		if err := p.call(ctx, endpoint, "2014-05-26", "DescribeDisks", query, &page); err != nil {
			return "", err
		}
		if page.Disks == nil {
			return "", errors.New("DescribeDisks: missing disk collection")
		}
		for _, disk := range page.Disks.Disk {
			attached := map[string]bool{disk.InstanceID: true}
			for _, a := range disk.Attachments.Attachment {
				attached[a.InstanceID] = true
			}
			for _, a := range disk.MountInstance {
				attached[a.InstanceID] = true
			}
			for id := range attached {
				if snapshot, ok := instances[id]; ok {
					snapshot.StorageGiB += disk.Size
					instances[id] = snapshot
				}
			}
		}
		return page.NextToken, nil
	})
	if err != nil {
		return nil, err
	}

	for ids := range slices.Chunk(subscriptions, 50) {
		var page struct {
			InstanceRenewAttributes *struct {
				InstanceRenewAttribute []struct {
					InstanceID, RenewalStatus string
					AutoRenewEnabled          *bool
					Duration                  int
					PeriodUnit                string
				}
			}
		}
		query := url.Values{"RegionId": {region}, "InstanceId": {strings.Join(ids, ",")}, "PageNumber": {"1"}, "PageSize": {"50"}}
		if err := p.call(ctx, endpoint, "2014-05-26", "DescribeInstanceAutoRenewAttribute", query, &page); err != nil {
			return nil, err
		}
		if page.InstanceRenewAttributes == nil {
			return nil, errors.New("DescribeInstanceAutoRenewAttribute: missing attribute collection")
		}
		for _, item := range page.InstanceRenewAttributes.InstanceRenewAttribute {
			if snapshot, ok := instances[item.InstanceID]; ok {
				snapshot.AutoRenew, snapshot.RenewalStatus = item.AutoRenewEnabled, item.RenewalStatus
				snapshot.RenewalCycle, snapshot.RenewalCycleCount = item.PeriodUnit, item.Duration
				instances[item.InstanceID] = snapshot
			}
		}
	}
	return instances, nil
}

func (p aliyunProvider) bills(ctx context.Context, month string) ([]cloudBill, error) {
	var lines []cloudBill
	endpoint := "business.aliyuncs.com"
	if p.account.BillingSite == "international" {
		endpoint = "business.ap-southeast-1.aliyuncs.com"
	}
	err := paginate(func(token string) (string, error) {
		var page struct {
			Data *struct {
				NextToken, BillingCycle string
				Items                   []struct {
					InstanceID, Currency, OwnerID   string
					PretaxAmount, PretaxGrossAmount json.Number
				}
			}
		}
		query := url.Values{"BillingCycle": {month}, "ProductCode": {"ecs"}, "Granularity": {"MONTHLY"}, "MaxResults": {"300"}, "BillOwnerId": {p.account.Account}}
		if token != "" {
			query.Set("NextToken", token)
		}
		if err := p.call(ctx, endpoint, "2017-12-14", "DescribeInstanceBill", query, &page); err != nil {
			return "", err
		}
		if page.Data == nil || (page.Data.BillingCycle != "" && page.Data.BillingCycle != month) {
			return "", errors.New("DescribeInstanceBill: invalid billing period")
		}
		for _, row := range page.Data.Items {
			if row.OwnerID != "" && row.OwnerID != p.account.Account {
				continue
			}
			lines = append(lines, cloudBill{row.InstanceID, row.Currency, row.PretaxAmount, row.PretaxGrossAmount})
		}
		return page.Data.NextToken, nil
	})
	return lines, err
}
