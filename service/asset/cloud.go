package asset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/nezhahq/nezha/model"
)

type cloudAccount struct {
	Provider        string   `yaml:"provider"`
	Account         string   `yaml:"account"`
	Label           string   `yaml:"label"`
	Regions         []string `yaml:"regions"`
	OwnerUserID     uint64   `yaml:"owner_user_id"`
	Interval        int      `yaml:"interval_minutes"`
	BillingMonths   int      `yaml:"billing_months"`
	BillingSite     string   `yaml:"billing_site"`
	AccessKeyID     string   `yaml:"access_key_id" json:"-"`
	AccessKeySecret string   `yaml:"access_key_secret" json:"-"`
	SecurityToken   string   `yaml:"security_token" json:"-"`
}

type accountKey struct{ provider, account string }

type CloudManager struct {
	db       *gorm.DB
	accounts []cloudAccount
	mu       sync.Mutex
	running  map[accountKey]bool
	http     *http.Client
}

var regionPattern = regexp.MustCompile(`^[a-z]{2,5}(?:-[a-z0-9]+){1,5}$`)
var accountPattern = regexp.MustCompile(`^[0-9]{16}$`)

func NewCloudManager(db *gorm.DB, path string) (*CloudManager, error) {
	m := &CloudManager{db: db, running: make(map[accountKey]bool), http: &http.Client{
		Timeout:       25 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	if path == "" {
		return m, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return m, errors.New("cannot open cloud configuration file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256*1024 || info.Mode().Perm()&0077 != 0 {
		return m, errors.New("cloud configuration must be a regular file of at most 256 KiB with owner-only access")
	}
	var config struct {
		Accounts []cloudAccount `yaml:"accounts"`
	}
	decoder := yaml.NewDecoder(io.LimitReader(file, 256*1024))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil || decoder.Decode(new(any)) != io.EOF {
		return m, errors.New("invalid cloud configuration YAML")
	}
	if len(config.Accounts) > 50 {
		return m, errors.New("at most 50 cloud accounts can be configured")
	}
	seen := make(map[accountKey]bool)
	for i := range config.Accounts {
		a := &config.Accounts[i]
		key := accountKey{a.Provider, a.Account}
		if a.Provider != aliyun || !accountPattern.MatchString(a.Account) || a.AccessKeyID == "" || a.AccessKeySecret == "" || len(a.Regions) == 0 || len(a.Regions) > 50 || seen[key] {
			return m, fmt.Errorf("invalid cloud account at position %d", i+1)
		}
		seen[key] = true
		slices.Sort(a.Regions)
		a.Regions = slices.Compact(a.Regions)
		for _, region := range a.Regions {
			if !regionPattern.MatchString(region) {
				return m, fmt.Errorf("invalid region in cloud account at position %d", i+1)
			}
		}
		if a.Interval == 0 {
			a.Interval = 360
		}
		if a.Interval < 15 || a.Interval > 10080 || a.BillingMonths < 0 || a.BillingMonths > 18 ||
			!slices.Contains([]string{"", "china", "international"}, a.BillingSite) {
			return m, fmt.Errorf("invalid interval or billing options in cloud account at position %d", i+1)
		}
		var owner model.User
		query := db.Order("id")
		if a.OwnerUserID == 0 {
			query = query.Where("role = ?", model.RoleAdmin)
		} else {
			query = query.Where("id = ?", a.OwnerUserID)
		}
		if err := query.First(&owner).Error; err != nil {
			return m, fmt.Errorf("cloud account at position %d has no valid asset owner", i+1)
		}
		a.OwnerUserID = owner.ID
	}
	m.accounts = config.Accounts
	return m, nil
}

func (m *CloudManager) Status(ctx context.Context) ([]model.CloudAccountView, error) {
	var statuses []model.AssetSyncStatus
	if err := m.db.WithContext(ctx).Find(&statuses).Error; err != nil {
		return nil, err
	}
	byKey := make(map[accountKey]model.AssetSyncStatus)
	for _, status := range statuses {
		byKey[accountKey{status.Provider, status.Account}] = status
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.CloudAccountView, 0, len(m.accounts))
	for _, a := range m.accounts {
		key := accountKey{a.Provider, a.Account}
		s := byKey[key]
		s.Provider, s.Account = a.Provider, a.Account
		out = append(out, model.CloudAccountView{AssetSyncStatus: s, Label: a.Label, Regions: a.Regions, Interval: a.Interval, BillingMonths: a.BillingMonths, Running: m.running[key]})
	}
	return out, nil
}

func (m *CloudManager) Start(providerName, account string) (bool, error) {
	for _, a := range m.accounts {
		if a.Provider == providerName && a.Account == account {
			return m.start(a, true), nil
		}
	}
	return false, errors.New("cloud account not configured")
}

func (m *CloudManager) RunDue() {
	statuses, err := m.Status(context.Background())
	if err != nil {
		log.Printf("NEZHA>> Unable to read cloud sync status")
		return
	}
	now := time.Now()
	for i, status := range statuses {
		if status.LastAttemptAt != nil && now.Sub(*status.LastAttemptAt) < 14*time.Minute {
			continue
		}
		inventoryDue := status.LastInventoryAt == nil || status.Error != "" || now.Sub(*status.LastInventoryAt) >= time.Duration(status.Interval)*time.Minute
		billingDue := status.BillingMonths != 0 && (status.LastBillingAt == nil || status.BillingError != "" || now.Sub(*status.LastBillingAt) >= 24*time.Hour)
		if inventoryDue || billingDue {
			m.start(m.accounts[i], false)
		}
	}
}

func (m *CloudManager) start(a cloudAccount, force bool) bool {
	key := accountKey{a.Provider, a.Account}
	m.mu.Lock()
	if m.running[key] {
		m.mu.Unlock()
		return false
	}
	m.running[key] = true
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.running, key)
			m.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var p provider = aliyunProvider{m.http, a}
		m.sync(ctx, a, p, force)
	}()
	return true
}

func (m *CloudManager) sync(ctx context.Context, a cloudAccount, p provider, force bool) {
	s := model.AssetSyncStatus{Provider: a.Provider, Account: a.Account}
	db := m.db.WithContext(ctx)
	if err := db.Where("provider = ? AND account = ?", a.Provider, a.Account).Find(&s).Error; err != nil {
		log.Print("NEZHA>> Unable to load cloud sync status")
		return
	}
	now := time.Now().UTC()
	s.LastAttemptAt = &now
	persist := func() {
		// Persist a useful failure status even if the provider's job timed out.
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.db.WithContext(writeCtx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&s).Error; err != nil {
			log.Print("NEZHA>> Unable to persist cloud sync status")
		}
	}
	persist()
	if err := p.verify(ctx); err != nil {
		s.Error = err.Error()
		persist()
		return
	}
	if force || s.LastInventoryAt == nil || s.Error != "" || now.Sub(*s.LastInventoryAt) >= time.Duration(a.Interval)*time.Minute {
		var failures []string
		for _, region := range a.Regions {
			if ctx.Err() != nil {
				failures = append(failures, "inventory timed out")
				break
			}
			results, err := p.inventory(ctx, region)
			if err != nil {
				failures = append(failures, region+": "+err.Error())
				continue
			}
			conflicts, err := saveInventory(db, a, region, results, now)
			if err != nil {
				failures = append(failures, region+": database write failed")
			} else if conflicts > 0 {
				failures = append(failures, fmt.Sprintf("%s: %d matching asset(s) belong to a different owner", region, conflicts))
			}
		}
		s.Error = strings.Join(failures, "; ")
		if len(failures) == 0 {
			s.LastInventoryAt = &now
		}
		persist()
	}
	if a.BillingMonths > 0 && (force || s.LastBillingAt == nil || s.BillingError != "" || now.Sub(*s.LastBillingAt) >= 24*time.Hour) {
		months := a.BillingMonths
		if !force && s.LastBillingAt != nil {
			months = min(2, months)
		}
		s.BillingError = ""
		// Compute months from the first day to avoid AddDate normalization at month-end.
		billingNow := now.In(time.FixedZone("UTC+8", 8*60*60))
		firstDay := time.Date(billingNow.Year(), billingNow.Month(), 1, 0, 0, 0, 0, billingNow.Location())
		for i := 0; i < months; i++ {
			month := firstDay.AddDate(0, -i, 0).Format("2006-01")
			lines, err := p.bills(ctx, month)
			if err != nil {
				s.BillingError = month + ": " + err.Error()
				break
			}
			if err := saveBills(db, a, month, lines, now); err != nil {
				s.BillingError = month + ": unable to store billing data"
				break
			}
		}
		if s.BillingError == "" {
			s.LastBillingAt = &now
		}
		persist()
	}
}

func saveInventory(db *gorm.DB, account cloudAccount, region string, results map[string]model.AssetCloud, now time.Time) (int, error) {
	conflicts := 0
	err := db.Transaction(func(tx *gorm.DB) error {
		var previous []model.Asset
		if err := tx.Unscoped().Where("provider = ? AND account = ? AND region = ? AND identity IS NOT NULL", account.Provider, account.Account, region).Find(&previous).Error; err != nil {
			return err
		}
		for _, a := range previous {
			snapshot, found := results[a.InstanceID]
			delete(results, a.InstanceID)
			if a.DeletedAt.Valid || (!found && (a.Cloud == nil || a.Cloud.Missing)) {
				continue
			}
			if found {
				if a.Cloud == nil && a.UserID != account.OwnerUserID {
					conflicts++
					continue
				}
				snapshot.SeenAt = now
			} else {
				snapshot = *a.Cloud
				snapshot.Missing = true
			}
			a.Cloud = &snapshot
			if err := tx.Model(&a).Select("Cloud").Updates(&a).Error; err != nil {
				return err
			}
		}
		for id, snapshot := range results {
			snapshot.SeenAt = now
			a := model.Asset{Common: model.Common{UserID: account.OwnerUserID}, Source: "cloud",
				Identity: Identity(account.Provider, account.Account, region, id), Cloud: &snapshot,
				AssetFields: model.AssetFields{Provider: account.Provider, Account: account.Account, Region: region, InstanceID: id, Kind: "cloud", Billing: "unknown"}}
			if err := tx.Create(&a).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return conflicts, err
}

func saveBills(db *gorm.DB, account cloudAccount, month string, lines []cloudBill, now time.Time) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var rows []model.Asset
		if err := tx.Unscoped().Where("provider = ? AND account = ? AND cloud IS NOT NULL", account.Provider, account.Account).Find(&rows).Error; err != nil {
			return err
		}
		ids := make(map[string]uint64, len(rows))
		for _, a := range rows {
			if _, exists := ids[a.InstanceID]; exists {
				ids[a.InstanceID] = 0 // The bill has no stable region identifier.
			} else if a.DeletedAt.Valid {
				ids[a.InstanceID] = 0
			} else {
				ids[a.InstanceID] = a.ID
			}
		}
		type key struct {
			assetID  uint64
			currency string
		}
		type sums struct{ payable, gross *big.Rat }
		totals := make(map[key]sums)
		for _, line := range lines {
			id := ids[line.InstanceID]
			if id == 0 {
				continue
			}
			if !currencyPattern.MatchString(line.Currency) {
				return errors.New("invalid bill currency")
			}
			k := key{id, line.Currency}
			s := totals[k]
			if s.payable == nil {
				s.payable = new(big.Rat)
			}
			if amount, ok := new(big.Rat).SetString(string(line.Amount)); ok {
				s.payable.Add(s.payable, amount)
			} else {
				return errors.New("invalid bill payable amount")
			}
			if line.Gross != "" {
				if s.gross == nil {
					s.gross = new(big.Rat)
				}
				if amount, ok := new(big.Rat).SetString(string(line.Gross)); ok {
					s.gross.Add(s.gross, amount)
				} else {
					return errors.New("invalid bill original amount")
				}
			}
			totals[k] = s
		}
		bills := make([]model.AssetBill, 0, len(totals))
		for k, s := range totals {
			value, err := Micros(s.payable.FloatString(6))
			if err != nil {
				return err
			}
			bill := model.AssetBill{AssetID: k.assetID, Provider: account.Provider, Account: account.Account, Month: month, Currency: k.currency, Micros: value, UpdatedAt: now}
			if s.gross != nil {
				gross, err := Micros(s.gross.FloatString(6))
				if err != nil {
					return err
				}
				bill.GrossMicros = &gross
			}
			bills = append(bills, bill)
		}
		if err := tx.Where("provider = ? AND account = ? AND month = ?", account.Provider, account.Account, month).Delete(&model.AssetBill{}).Error; err != nil {
			return err
		}
		if len(bills) > 0 {
			return tx.Create(&bills).Error
		}
		return nil
	})
}
