package model

import "time"

type AssetMonitor struct {
	Name       string     `json:"name"`
	Online     bool       `json:"online"`
	LastActive time.Time  `json:"last_active"`
	Host       *Host      `json:"host"`
	State      *HostState `json:"state"`
	AlertCount int        `json:"alert_count"`
}

type AssetView struct {
	*Asset
	DisplayName      string        `json:"display_name"`
	EffectiveExpiry  *time.Time    `json:"effective_expiry"`
	DaysRemaining    *int          `json:"days_remaining"`
	EffectiveBilling string        `json:"effective_billing"`
	Monitor          *AssetMonitor `json:"monitor,omitempty"`
}

type AssetList struct {
	Assets     []AssetView    `json:"assets"`
	Count      int            `json:"count"`
	Expiring7  int            `json:"expiring_7"`
	Expiring15 int            `json:"expiring_15"`
	Expiring30 int            `json:"expiring_30"`
	Providers  map[string]int `json:"providers"`
	Regions    map[string]int `json:"regions"`
}

type AssetChargeView struct {
	GrossAmount string    `json:"gross_amount,omitempty"`
	Month       string    `json:"month"`
	Amount      string    `json:"amount"`
	Currency    string    `json:"currency"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type AssetHistoryView struct {
	Renewals []AssetRenewal    `json:"renewals"`
	Bills    []AssetChargeView `json:"bills"`
}

type AssetRenewalForm struct {
	PaidAt    time.Time `json:"paid_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Amount    string    `json:"amount"`
	Currency  string    `json:"currency"`
	Note      string    `json:"note"`
}

type AssetCostRow struct {
	Provider         string `json:"provider"`
	Project          string `json:"project"`
	Currency         string `json:"currency"`
	CloudGross       string `json:"cloud_gross,omitempty"`
	CloudBilled      string `json:"cloud_billed,omitempty"`
	ManualPurchase   string `json:"manual_purchase,omitempty"`
	ManualRenewal    string `json:"manual_renewal,omitempty"`
	EstimatedMonthly string `json:"estimated_monthly,omitempty"`
	EstimatedAnnual  string `json:"estimated_annual,omitempty"`
}

type AssetCloudStatusView struct {
	Accounts    []CloudAccountView `json:"accounts"`
	ConfigError string             `json:"config_error,omitempty"`
}

type AssetCloudSyncResult struct {
	Started     bool      `json:"started"`
	RequestedAt time.Time `json:"requested_at"`
}

type CloudAccountView struct {
	AssetSyncStatus
	Label         string   `json:"label"`
	Regions       []string `json:"regions"`
	Interval      int      `json:"interval_minutes"`
	BillingMonths int      `json:"billing_months"`
	Running       bool     `json:"running"`
}
