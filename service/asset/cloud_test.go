package asset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nezhahq/nezha/model"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAliyunReadOnlyRequests(t *testing.T) {
	now := time.Now()
	a := cloudAccount{Account: "1234567890123456", AccessKeyID: "test", AccessKeySecret: "example"}
	seen := make(map[string]int)
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		action := q.Get("Action")
		seen[action]++
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "https", r.URL.Scheme)
		signature := q.Get("Signature")
		q.Del("Signature")
		require.Equal(t, rpcSignature(q, a.AccessKeySecret), signature)
		require.WithinDuration(t, now, *ParseDate(q.Get("Timestamp")), 30*time.Second)
		var body string
		switch action {
		case "GetCallerIdentity":
			require.Equal(t, "sts.aliyuncs.com", r.URL.Host)
			body = `{"AccountId":"1234567890123456","IdentityType":"RAMUser"}`
		case "DescribeInstances":
			require.Equal(t, "ecs.cn-hangzhou.aliyuncs.com", r.URL.Host)
			if q.Get("NextToken") == "" {
				body = `{"NextToken":"page+2 /","Instances":{"Instance":[{"InstanceId":"i-sub","InstanceName":"host","Cpu":2,"Memory":2048,"InstanceChargeType":"PrePaid","ExpiredTime":"2026-12-01T09:20Z","VpcAttributes":{"PrivateIpAddress":{"IpAddress":["10.0.0.1"]}},"EipAddress":{"IpAddress":"192.0.2.1","Bandwidth":5}}]}}`
			} else {
				require.Equal(t, "page+2 /", q.Get("NextToken"))
				body = `{"Instances":{"Instance":[{"InstanceId":"i-paygo","InstanceChargeType":"PostPaid","ExpiredTime":"2026-12-01T09:20Z"}]}}`
			}
		case "DescribeDisks":
			body = `{"Disks":{"Disk":[{"InstanceId":"i-sub","Size":20,"Attachments":{"Attachment":[{"InstanceId":"i-sub"},{"InstanceId":"i-paygo"}]}}]}}`
		case "DescribeInstanceAutoRenewAttribute":
			require.Equal(t, "i-sub", q.Get("InstanceId"))
			body = `{"InstanceRenewAttributes":{"InstanceRenewAttribute":[{"InstanceId":"i-sub","AutoRenewEnabled":true,"Duration":1,"PeriodUnit":"month","RenewalStatus":"AutoRenewal"}]}}`
		case "DescribeInstanceBill":
			require.Equal(t, "business.aliyuncs.com", r.URL.Host)
			require.Equal(t, a.Account, q.Get("BillOwnerId"))
			if q.Get("NextToken") == "" {
				body = `{"Code":"Success","Success":true,"Data":{"BillingCycle":"2026-11","NextToken":"2","Items":[{"InstanceID":"i-sub","OwnerID":"1234567890123456","Currency":"CNY","PretaxAmount":12.2,"PretaxGrossAmount":13},{"InstanceID":"i-sub","OwnerID":"9999999999999999","Currency":"CNY","PretaxAmount":90}]}}`
			} else {
				body = `{"Data":{"Items":[{"InstanceID":"i-sub","Currency":"CNY","PretaxAmount":-0.2,"PretaxGrossAmount":-0.3}]}}`
			}
		default:
			t.Fatalf("unexpected action %s", action)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	p := aliyunProvider{client, a}
	ctx := context.Background()
	require.NoError(t, p.verify(ctx))
	items, err := p.inventory(ctx, "cn-hangzhou")
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, "192.0.2.1", items["i-sub"].PublicIPs)
	require.Equal(t, "10.0.0.1", items["i-sub"].PrivateIPs)
	require.Equal(t, int64(20), items["i-sub"].StorageGiB)
	require.True(t, *items["i-sub"].AutoRenew)
	require.Equal(t, "2026-12-01T09:20:00Z", items["i-sub"].ExpiresAt.Format(time.RFC3339))
	require.Nil(t, items["i-paygo"].ExpiresAt)
	lines, err := p.bills(ctx, "2026-11")
	require.NoError(t, err)
	require.Len(t, lines, 2)
	require.Equal(t, 2, seen["DescribeInstances"])
	require.Equal(t, 2, seen["DescribeInstanceBill"])
}

type fixtureProvider struct {
	results      map[string]model.AssetCloud
	inventoryErr error
	lines        []cloudBill
	billErr      error
}

func (f fixtureProvider) verify(context.Context) error { return nil }
func (f fixtureProvider) inventory(context.Context, string) (map[string]model.AssetCloud, error) {
	return f.results, f.inventoryErr
}
func (f fixtureProvider) bills(context.Context, string) ([]cloudBill, error) {
	return f.lines, f.billErr
}

func TestSyncKeepsManualDataAndLastGoodBills(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Asset{}, &model.AssetBill{}, &model.AssetSyncStatus{}))
	m := &CloudManager{db: db}
	ctx := context.Background()
	a := cloudAccount{Provider: aliyun, Account: "1234567890123456", OwnerUserID: 4, Regions: []string{"cn-hangzhou"}, BillingMonths: 1, Interval: 15}
	creation := model.Asset{Common: model.Common{UserID: 4}, AssetFields: model.AssetFields{Name: "manual name", Note: "private note", Provider: aliyun, Account: a.Account,
		Region: "cn-hangzhou", InstanceID: "i-1", AssetPlan: model.AssetPlan{CPUCores: 12}, Project: "project", RenewalPrice: "50", Currency: "CNY"}, Identity: Identity(aliyun, a.Account, "cn-hangzhou", "i-1")}
	require.NoError(t, db.Create(&creation).Error)
	first := fixtureProvider{results: map[string]model.AssetCloud{"i-1": {Name: "cloud name", Billing: "subscription", AssetPlan: model.AssetPlan{CPUCores: 2}}, "i-2": {Name: "new instance"}},
		lines: []cloudBill{{"i-1", "CNY", "12.2", "13"}, {"i-1", "CNY", "-0.2", "-0.3"}, {"i-1", "USD", "1.5", "2"}, {"i-unknown", "USD", "100", "100"}}}
	m.sync(ctx, a, first, true)
	var reloaded model.Asset
	require.NoError(t, db.First(&reloaded, creation.ID).Error)
	require.Equal(t, "manual name", reloaded.Name)
	require.Equal(t, "private note", reloaded.Note)
	require.Equal(t, "project", reloaded.Project)
	require.Equal(t, 12, reloaded.CPUCores)
	require.Equal(t, 2, reloaded.Cloud.CPUCores)
	require.Equal(t, "50", reloaded.RenewalPrice)
	var invoices []model.AssetBill
	require.NoError(t, db.Order("currency").Find(&invoices).Error)
	require.Len(t, invoices, 2)
	require.Equal(t, int64(12_000_000), invoices[0].Micros)
	require.Equal(t, int64(12_700_000), *invoices[0].GrossMicros)
	require.Equal(t, "USD", invoices[1].Currency)
	var status model.AssetSyncStatus
	require.NoError(t, db.First(&status).Error)
	require.NotNil(t, status.LastInventoryAt)
	require.NotNil(t, status.LastBillingAt)

	m.sync(ctx, a, fixtureProvider{inventoryErr: errors.New("limited"), billErr: errors.New("temporarily unavailable")}, true)
	require.NoError(t, db.First(&reloaded, creation.ID).Error)
	require.False(t, reloaded.Cloud.Missing)
	require.NoError(t, db.First(&status).Error)
	require.Contains(t, status.Error, "limited")
	require.Contains(t, status.BillingError, "temporarily unavailable")
	var count int64
	require.NoError(t, db.Model(&model.AssetBill{}).Count(&count).Error)
	require.EqualValues(t, 2, count)

	var synced model.Asset
	require.NoError(t, db.Where("instance_id = ?", "i-2").First(&synced).Error)
	require.NoError(t, db.Delete(&synced).Error)
	restore := fixtureProvider{results: map[string]model.AssetCloud{"i-2": {Name: "still exists"}}, billErr: fmt.Errorf("test leaves billing intact")}
	m.sync(ctx, a, restore, true)
	require.NoError(t, db.First(&reloaded, creation.ID).Error)
	require.True(t, reloaded.Cloud.Missing)
	require.NoError(t, db.Model(&model.Asset{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestCloudSyncRejectsAnIdentityClaimFromAnotherOwner(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Asset{}, &model.AssetBill{}))
	a := cloudAccount{Provider: aliyun, Account: "1234567890123456", OwnerUserID: 1}
	region := "cn-hangzhou"
	claim := model.Asset{Common: model.Common{UserID: 27}, Identity: Identity(aliyun, a.Account, region, "i-1"),
		AssetFields: model.AssetFields{Provider: aliyun, Account: a.Account, Region: region, InstanceID: "i-1"}}
	require.NoError(t, db.Create(&claim).Error)
	now := time.Now()
	conflicts, err := saveInventory(db, a, region, map[string]model.AssetCloud{"i-1": {Name: "private snapshot"}, "i-safe": {Name: "unrelated"}}, now)
	require.NoError(t, err)
	require.Equal(t, 1, conflicts)
	require.NoError(t, saveBills(db, a, "2026-11", []cloudBill{{"i-1", "CNY", "90", "100"}}, now))
	var read model.Asset
	require.NoError(t, db.First(&read, claim.ID).Error)
	require.Nil(t, read.Cloud)
	var count int64
	require.NoError(t, db.Model(&model.AssetBill{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Model(&model.Asset{}).Where("instance_id = ?", "i-safe").Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestImportLegacyAmounts(t *testing.T) {
	a, err := FromPublicNote("host", `{"billingDataMod":{"startDate":"2026-01-01","endDate":"2027-01-01","amount":"200EUR","cycle":"Year","autoRenewal":"1"},"planDataMod":{"bandwidth":"100Mbps","trafficVol":"1TB"}}`, 3, 7)
	require.NoError(t, err)
	require.Equal(t, "200", a.PurchasePrice)
	require.Equal(t, "EUR", a.Currency)
	require.Equal(t, "year", a.Cycle)
	require.Equal(t, "100Mbps", a.Bandwidth)
	require.EqualValues(t, 7, *a.ServerID)
	require.True(t, *a.AutoRenew)
	a, err = FromPublicNote("host", `{"billingDataMod":{"endDate":"0000-00-00T23:59:59+08:00","amount":"200"}}`, 3, 7)
	require.NoError(t, err)
	require.Equal(t, "permanent", a.Billing)
	require.Nil(t, a.EffectiveExpiry())
	require.Empty(t, a.PurchasePrice)
	require.Equal(t, "200", a.LegacyPrice)
}
