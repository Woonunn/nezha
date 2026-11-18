package controller

import (
	"errors"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	assets "github.com/nezhahq/nezha/service/asset"
	"github.com/nezhahq/nezha/service/singleton"
)

// @Summary List manual renewals and cloud bills
// @Tags Asset
// @Security APITokenAuth
// @Param id path int true "Asset ID"
// @Success 200 {object} model.CommonResponse[model.AssetHistoryView]
// @Router /asset/{id}/history [get]
func assetHistory(c *gin.Context) (model.AssetHistoryView, error) {
	id, err := assetID(c)
	if err != nil {
		return model.AssetHistoryView{}, err
	}
	db := singleton.DB.WithContext(c.Request.Context())
	if _, err := readAsset(c, db, id); err != nil {
		return model.AssetHistoryView{}, err
	}
	out := model.AssetHistoryView{Renewals: make([]model.AssetRenewal, 0), Bills: make([]model.AssetChargeView, 0)}
	if err := db.Where("asset_id = ?", id).Order("paid_at DESC, id DESC").Find(&out.Renewals).Error; err != nil {
		return out, newGormError("read renewals: %v", err)
	}
	var bills []model.AssetBill
	if err := db.Where("asset_id = ?", id).Order("month DESC, currency").Find(&bills).Error; err != nil {
		return out, newGormError("read bills: %v", err)
	}
	for _, b := range bills {
		charge := model.AssetChargeView{Month: b.Month, Amount: big.NewRat(b.Micros, 1_000_000).FloatString(6), Currency: b.Currency, UpdatedAt: b.UpdatedAt}
		if b.GrossMicros != nil {
			charge.GrossAmount = big.NewRat(*b.GrossMicros, 1_000_000).FloatString(6)
		}
		out.Bills = append(out.Bills, charge)
	}
	return out, nil
}

// @Summary Record a manual renewal
// @Tags Asset
// @Security APITokenAuth
// @Param id path int true "Asset ID"
// @Param body body model.AssetRenewalForm true "Asset fields"
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /asset/{id}/renewals [post]
func addAssetRenewal(c *gin.Context) (uint64, error) {
	id, err := assetID(c)
	if err != nil {
		return 0, err
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32*1024)
	var f model.AssetRenewalForm
	if err := c.ShouldBindJSON(&f); err != nil || f.PaidAt.IsZero() || !f.ExpiresAt.After(f.PaidAt) || len(f.Note) > 4096 {
		return 0, errors.New("invalid renewal dates or note")
	}
	f.Currency = strings.ToUpper(strings.TrimSpace(f.Currency))
	if err := assets.ValidateMoney(f.Amount, f.Currency); err != nil {
		return 0, err
	}
	renewal := model.AssetRenewal{AssetID: id, PaidAt: f.PaidAt.UTC(), ExpiresAt: f.ExpiresAt.UTC(), Amount: f.Amount, Currency: f.Currency, Note: f.Note}
	err = singleton.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		a, err := readAsset(c, tx, id)
		if err != nil {
			return err
		}
		var existing model.AssetRenewal
		err = tx.Where("asset_id = ? AND expires_at = ?", id, renewal.ExpiresAt).First(&existing).Error
		if err == nil {
			if existing.PaidAt.Equal(renewal.PaidAt) && existing.Amount == renewal.Amount && existing.Currency == renewal.Currency && existing.Note == renewal.Note {
				renewal.ID = existing.ID
				return nil
			}
			return errors.New("a renewal for this expiry already exists")
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return assetWriteError(err)
		}
		if err := tx.Create(&renewal).Error; err != nil {
			return assetWriteError(err)
		}
		if a.ExpiresAt == nil || renewal.ExpiresAt.After(*a.ExpiresAt) {
			return assetWriteErrorOrNil(tx.Model(a).Update("expires_at", renewal.ExpiresAt).Error)
		}
		return nil
	})
	return renewal.ID, err
}

type assetCostFilters struct {
	Period   string `form:"period" json:"period"`
	Provider string `form:"provider" json:"provider"`
	Project  string `form:"project" json:"project"`
}

func costReport(c *gin.Context, filters assetCostFilters) ([]model.AssetCostRow, error) {
	if filters.Period == "" {
		filters.Period = time.Now().Format("2006-01")
	}
	start, end, err := assets.ParsePeriod(filters.Period)
	if err != nil {
		return nil, err
	}
	rows, err := inventoryQuery(c, assetFilters{Provider: filters.Provider, Project: filters.Project})
	if err != nil {
		return nil, err
	}
	type key struct{ provider, project, currency string }
	type amount struct{ billed, gross, purchase, renewal, annual *big.Rat }
	totals := make(map[key]*amount)
	group := func(a *model.Asset, currency string) *amount {
		k := key{a.Provider, a.Project, currency}
		if totals[k] == nil {
			totals[k] = &amount{}
		}
		return totals[k]
	}
	add := func(sum **big.Rat, value *big.Rat) {
		if *sum == nil {
			*sum = new(big.Rat)
		}
		(*sum).Add(*sum, value)
	}
	ids := make([]uint64, 0, len(rows))
	byID := make(map[uint64]*model.Asset, len(rows))
	for _, a := range rows {
		ids = append(ids, a.ID)
		byID[a.ID] = a
		if a.PurchasePrice != "" && a.Currency != "" && a.PurchasedAt != nil && !a.PurchasedAt.Before(start) && a.PurchasedAt.Before(end) {
			if price, ok := new(big.Rat).SetString(a.PurchasePrice); ok {
				add(&group(a, a.Currency).purchase, price)
			}
		}
		if a.EffectiveBilling() == "subscription" && a.Status != "retired" && (a.Cloud == nil || !a.Cloud.Missing) {
			price := a.RenewalPrice
			if price == "" {
				price = a.PurchasePrice
			}
			if factor := assets.CycleYearFactor(a.Cycle, a.CycleCount); factor != nil && a.Currency != "" {
				if raw, ok := new(big.Rat).SetString(price); ok {
					add(&group(a, a.Currency).annual, raw.Mul(raw, factor))
				}
			}
		}
	}
	db := singleton.DB.WithContext(c.Request.Context())
	for batch := range slices.Chunk(ids, 500) {
		var renewals []model.AssetRenewal
		if err := db.Where("asset_id IN ? AND paid_at >= ? AND paid_at < ?", batch, start, end).Find(&renewals).Error; err != nil {
			return nil, newGormError("read renewal costs: %v", err)
		}
		for _, r := range renewals {
			if price, ok := new(big.Rat).SetString(r.Amount); ok {
				add(&group(byID[r.AssetID], r.Currency).renewal, price)
			}
		}
		var bills []model.AssetBill
		if err := db.Where("asset_id IN ? AND month >= ? AND month < ?", batch, start.Format("2006-01"), end.Format("2006-01")).Find(&bills).Error; err != nil {
			return nil, newGormError("read cloud costs: %v", err)
		}
		for _, b := range bills {
			sum := group(byID[b.AssetID], b.Currency)
			add(&sum.billed, big.NewRat(b.Micros, 1_000_000))
			if b.GrossMicros != nil {
				add(&sum.gross, big.NewRat(*b.GrossMicros, 1_000_000))
			}
		}
	}
	out := make([]model.AssetCostRow, 0, len(totals))
	format := func(v *big.Rat) string {
		if v == nil {
			return ""
		}
		return strings.TrimRight(strings.TrimRight(v.FloatString(6), "0"), ".")
	}
	for k, value := range totals {
		row := model.AssetCostRow{Provider: k.provider, Project: k.project, Currency: k.currency,
			CloudBilled: format(value.billed), CloudGross: format(value.gross), ManualPurchase: format(value.purchase), ManualRenewal: format(value.renewal), EstimatedAnnual: format(value.annual)}
		if value.annual != nil {
			row.EstimatedMonthly = format(new(big.Rat).Quo(value.annual, big.NewRat(12, 1)))
		}
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b model.AssetCostRow) int {
		if cmp := strings.Compare(a.Provider, b.Provider); cmp != 0 {
			return cmp
		}
		if cmp := strings.Compare(a.Project, b.Project); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Currency, b.Currency)
	})
	return out, nil
}

// @Summary Aggregate actual charges and recurring estimates by provider, project and currency
// @Tags Asset
// @Security APITokenAuth
// @Param period query string false "YYYY or YYYY-MM (default current month)"
// @Param provider query string false "Cloud provider"
// @Param project query string false "Project"
// @Success 200 {object} model.CommonResponse[[]model.AssetCostRow]
// @Router /asset/costs [get]
func assetCosts(c *gin.Context) ([]model.AssetCostRow, error) {
	var filters assetCostFilters
	if err := c.ShouldBindQuery(&filters); err != nil {
		return nil, errors.New("invalid cost filters")
	}
	return costReport(c, filters)
}
