package model

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// Asset is private inventory. Server continues to own the Agent's lifecycle.
type Asset struct {
	Common
	AssetFields
	DeletedAt   gorm.DeletedAt `json:"-" gorm:"index"`
	Identity    *string        `json:"-" gorm:"uniqueIndex"`
	Cloud       *AssetCloud    `json:"cloud,omitempty" gorm:"serializer:json"`
	Source      string         `json:"source"`
	LegacyPrice string         `json:"legacy_price,omitempty"`
}

type AssetFields struct {
	Name          string   `json:"name"`
	Purpose       string   `json:"purpose"`
	Provider      string   `json:"provider"`
	Account       string   `json:"account"`
	Region        string   `json:"region"`
	Zone          string   `json:"zone"`
	InstanceID    string   `json:"instance_id"`
	ServerID      *uint64  `json:"server_id" gorm:"uniqueIndex"`
	ParentID      *uint64  `json:"parent_id" gorm:"index"`
	GroupID       *uint64  `json:"group_id" gorm:"index"`
	Project       string   `json:"project"`
	Environment   string   `json:"environment"`
	Kind          string   `json:"kind"`
	Status        string   `json:"status"`
	Tags          []string `json:"tags" gorm:"serializer:json"`
	Note          string   `json:"note"`
	AssetPlan     `gorm:"embedded"`
	Billing       string     `json:"billing"`
	PurchasedAt   *time.Time `json:"purchased_at"`
	ExpiresAt     *time.Time `json:"expires_at"`
	AutoRenew     *bool      `json:"auto_renew"`
	PurchasePrice string     `json:"purchase_price"`
	RenewalPrice  string     `json:"renewal_price"`
	Currency      string     `json:"currency"`
	Cycle         string     `json:"cycle"`
	CycleCount    int        `json:"cycle_count"`
	ReminderGroup *uint64    `json:"reminder_group"`
	ReminderDays  []int      `json:"reminder_days" gorm:"serializer:json"`
}

type AssetPlan struct {
	PublicIPs  string `json:"public_ips"`
	PrivateIPs string `json:"private_ips"`
	Domains    string `json:"domains"`
	Bandwidth  string `json:"bandwidth"`
	Traffic    string `json:"traffic"`
	CPUCores   int    `json:"cpu_cores"`
	MemoryMiB  int64  `json:"memory_mib"`
	StorageGiB int64  `json:"storage_gib"`
	OS         string `json:"os"`
}

type AssetCloud struct {
	AssetPlan
	Name              string     `json:"name"`
	Zone              string     `json:"zone"`
	InstanceType      string     `json:"instance_type"`
	Status            string     `json:"status"`
	Billing           string     `json:"billing"`
	PurchasedAt       *time.Time `json:"purchased_at"`
	ExpiresAt         *time.Time `json:"expires_at"`
	AutoRenew         *bool      `json:"auto_renew"`
	RenewalCycle      string     `json:"renewal_cycle"`
	RenewalCycleCount int        `json:"renewal_cycle_count"`
	RenewalStatus     string     `json:"renewal_status"`
	SeenAt            time.Time  `json:"seen_at"`
	Missing           bool       `json:"missing"`
}

func (a Asset) EffectiveBilling() string {
	if a.Cloud != nil && a.Cloud.Billing != "unknown" && a.Cloud.Billing != "" {
		return a.Cloud.Billing
	}
	return a.Billing
}

func (a Asset) EffectiveExpiry() *time.Time {
	if a.Cloud != nil && a.Cloud.Billing != "unknown" && a.Cloud.Billing != "" {
		if a.Cloud.Billing == "on_demand" || a.Cloud.ExpiresAt != nil {
			return a.Cloud.ExpiresAt
		}
	}
	return a.ExpiresAt
}

func (a Asset) DisplayName() string {
	if a.Name == "" && a.Cloud != nil && a.Cloud.Name != "" {
		return a.Cloud.Name
	}
	if a.Name == "" {
		return a.InstanceID
	}
	return a.Name
}

type AssetRenewal struct {
	ID        uint64    `json:"id" gorm:"primaryKey"`
	AssetID   uint64    `json:"asset_id" gorm:"uniqueIndex:asset_renewal_key"`
	PaidAt    time.Time `json:"paid_at"`
	ExpiresAt time.Time `json:"expires_at" gorm:"uniqueIndex:asset_renewal_key"`
	Amount    string    `json:"amount"`
	Currency  string    `json:"currency"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

type AssetBill struct {
	ID          uint64    `json:"-" gorm:"primaryKey"`
	AssetID     uint64    `json:"asset_id" gorm:"index"`
	Provider    string    `json:"provider" gorm:"index:asset_bill_account,priority:1"`
	Account     string    `json:"account" gorm:"index:asset_bill_account,priority:2"`
	Month       string    `json:"month" gorm:"index:asset_bill_account,priority:3"`
	Currency    string    `json:"currency"`
	GrossMicros *int64    `json:"-"`
	Micros      int64     `json:"-"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type AssetReminder struct {
	AssetID   uint64    `gorm:"primaryKey;autoIncrement:false"`
	GroupID   uint64    `gorm:"primaryKey;autoIncrement:false"`
	ExpiresAt time.Time `gorm:"primaryKey"`
	Days      int       `gorm:"primaryKey;autoIncrement:false"`
	SentAt    time.Time
}

type AssetSyncStatus struct {
	Provider        string     `json:"provider" gorm:"primaryKey"`
	Account         string     `json:"account" gorm:"primaryKey"`
	LastAttemptAt   *time.Time `json:"last_attempt_at"`
	LastInventoryAt *time.Time `json:"last_inventory_at"`
	LastBillingAt   *time.Time `json:"last_billing_at"`
	Error           string     `json:"error"`
	BillingError    string     `json:"billing_error"`
}

var ErrAssetNotFound = errors.New("asset not found or access denied")
