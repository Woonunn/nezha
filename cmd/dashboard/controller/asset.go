package controller

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	assets "github.com/nezhahq/nezha/service/asset"
	"github.com/nezhahq/nezha/service/singleton"
)

type assetFilters struct {
	Q          string `form:"q" json:"q"`
	Provider   string `form:"provider" json:"provider"`
	Region     string `form:"region" json:"region"`
	Purpose    string `form:"purpose" json:"purpose"`
	Project    string `form:"project" json:"project"`
	Status     string `form:"status" json:"status"`
	Billing    string `form:"billing" json:"billing"`
	Tag        string `form:"tag" json:"tag"`
	GroupID    uint64 `form:"group_id" json:"group_id"`
	ServerID   uint64 `form:"server_id" json:"server_id"`
	ExpiryDays *int   `form:"expiry_days" json:"expiry_days"`
	Issue      string `form:"issue" json:"issue"`
	Sort       string `form:"sort" json:"sort"`
	Offset     int    `form:"offset" json:"offset"`
	Limit      int    `form:"limit" json:"limit"`
}

func (f assetFilters) validate() error {
	if f.ExpiryDays != nil && (*f.ExpiryDays < 0 || *f.ExpiryDays > 3650) {
		return errors.New("expiry_days must be between 0 and 3650")
	}
	if f.Offset < 0 || f.Limit < 0 || f.Limit > 200 || len(f.Q) > 512 {
		return errors.New("invalid search or pagination")
	}
	if !slices.Contains([]string{"", "name", "expiry", "updated"}, f.Sort) ||
		!slices.Contains([]string{"", "offline", "alert", "expired", "unmonitored"}, f.Issue) {
		return errors.New("invalid sort or issue")
	}
	return nil
}

func assetServer(c *gin.Context, id uint64) (*model.Server, bool) {
	if !patAllowsServer(c, id) || singleton.ServerShared == nil {
		return nil, false
	}
	server, found := singleton.ServerShared.Get(id)
	if !found || server == nil {
		return nil, false
	}
	return server, server.HasPermission(c) && (callerIsAdmin(c) || !singleton.ServerControlTransitioning(id))
}

func assetAllowed(c *gin.Context, a *model.Asset) bool {
	if !a.HasPermission(c) {
		return false
	}
	if a.ServerID == nil {
		return !patHasServerWhitelist(c)
	}
	server, allowed := assetServer(c, *a.ServerID)
	return allowed || (server == nil && !patHasServerWhitelist(c))
}

func readAsset(c *gin.Context, db *gorm.DB, id uint64) (*model.Asset, error) {
	var a model.Asset
	if err := db.First(&a, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrAssetNotFound
		}
		return nil, newGormError("read asset: %v", err)
	}
	if !assetAllowed(c, &a) {
		return nil, model.ErrAssetNotFound
	}
	return &a, nil
}

func assetID(c *gin.Context) (uint64, error) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		return 0, model.ErrAssetNotFound
	}
	return id, nil
}

func inventoryQuery(c *gin.Context, f assetFilters) ([]*model.Asset, error) {
	db := singleton.DB.WithContext(c.Request.Context())
	query := db.Model(&model.Asset{})
	if !callerIsAdmin(c) {
		query = query.Where("user_id = ?", getUid(c))
	}
	if f.ServerID != 0 {
		query = query.Where("server_id = ?", f.ServerID)
	}
	for column, value := range map[string]string{"provider": f.Provider, "region": f.Region, "purpose": f.Purpose, "project": f.Project} {
		if value != "" {
			query = query.Where("LOWER("+column+") = LOWER(?)", value)
		}
	}
	if f.GroupID != 0 {
		var group model.ServerGroup
		err := db.First(&group, f.GroupID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, newGormError("read asset group: %v", err)
		}
		if err != nil || !group.HasPermission(c) {
			return []*model.Asset{}, nil
		}
		members := db.Model(&model.ServerGroupServer{}).Select("server_id").Where("server_group_id = ?", f.GroupID)
		query = query.Where("group_id = ? OR server_id IN (?)", f.GroupID, members)
	}
	var rows []*model.Asset
	if err := query.Find(&rows).Error; err != nil {
		return nil, newGormError("list assets: %v", err)
	}
	return slices.DeleteFunc(rows, func(a *model.Asset) bool {
		if !assetAllowed(c, a) || (f.Tag != "" && !slices.Contains(a.Tags, f.Tag)) {
			return true
		}
		if f.Q == "" {
			return false
		}
		search := []string{a.DisplayName(), a.InstanceID, a.Purpose, a.Provider, a.Region, a.Project,
			a.PublicIPs, a.PrivateIPs, a.Domains, a.Environment, strings.Join(a.Tags, " ")}
		if a.Cloud != nil {
			search = append(search, a.Cloud.PublicIPs, a.Cloud.PrivateIPs, a.Cloud.Name)
		}
		return !strings.Contains(strings.ToLower(strings.Join(search, "\n")), strings.ToLower(f.Q))
	}), nil
}

func assetViewFor(a *model.Asset, now time.Time, visibleParents map[uint64]bool) model.AssetView {
	copy := *a
	if a.ParentID != nil && !visibleParents[*a.ParentID] {
		copy.ParentID = nil
	}
	out := model.AssetView{Asset: &copy, DisplayName: a.DisplayName(), EffectiveExpiry: a.EffectiveExpiry(), EffectiveBilling: a.EffectiveBilling()}
	if out.EffectiveExpiry != nil {
		days := assets.DaysRemaining(*out.EffectiveExpiry, now)
		out.DaysRemaining = &days
	}
	return out
}

func assetMonitoring(c *gin.Context, a *model.Asset, now time.Time, alerts map[uint64]int) *model.AssetMonitor {
	if a.ServerID == nil {
		return nil
	}
	server, ok := assetServer(c, *a.ServerID)
	if !ok {
		return nil
	}
	snapshot := server.RuntimeSnapshot()
	age := now.Sub(snapshot.LastActive)
	return &model.AssetMonitor{Name: server.Name, LastActive: snapshot.LastActive, Host: snapshot.Host, State: snapshot.State,
		Online: age >= 0 && age < 30*time.Second, AlertCount: alerts[server.ID]}
}

func buildAssetList(c *gin.Context, filters assetFilters) (model.AssetList, error) {
	if err := filters.validate(); err != nil {
		return model.AssetList{}, err
	}
	rows, err := inventoryQuery(c, filters)
	if err != nil {
		return model.AssetList{}, err
	}
	parentIDs := make([]uint64, 0)
	for _, a := range rows {
		if a.ParentID != nil {
			parentIDs = append(parentIDs, *a.ParentID)
		}
	}
	visibleParents := make(map[uint64]bool)
	if len(parentIDs) > 0 {
		var parents []*model.Asset
		if err := singleton.DB.WithContext(c.Request.Context()).Where("id IN ?", parentIDs).Find(&parents).Error; err != nil {
			return model.AssetList{}, newGormError("list parent assets: %v", err)
		}
		for _, p := range parents {
			visibleParents[p.ID] = assetAllowed(c, p)
		}
	}
	now := time.Now()
	alerts := singleton.ActiveAlertCounts(getUid(c), callerIsAdmin(c))
	out := model.AssetList{Assets: make([]model.AssetView, 0), Providers: make(map[string]int), Regions: make(map[string]int)}
	for _, a := range rows {
		view := assetViewFor(a, now, visibleParents)
		if (filters.Billing != "" && view.EffectiveBilling != filters.Billing) ||
			(filters.Status != "" && !strings.EqualFold(a.Status, filters.Status) && (a.Cloud == nil || !strings.EqualFold(a.Cloud.Status, filters.Status))) {
			continue
		}
		if filters.Issue == "offline" || filters.Issue == "alert" || filters.Issue == "unmonitored" {
			view.Monitor = assetMonitoring(c, a, now, alerts)
		}
		switch filters.Issue {
		case "offline":
			if view.Monitor == nil || view.Monitor.Online {
				continue
			}
		case "alert":
			if view.Monitor == nil || view.Monitor.AlertCount == 0 {
				continue
			}
		case "expired":
			if view.DaysRemaining == nil || *view.DaysRemaining >= 0 {
				continue
			}
		case "unmonitored":
			if view.Monitor != nil {
				continue
			}
		}
		upcoming := view.DaysRemaining != nil && *view.DaysRemaining >= 0 && a.Status != "retired" && (a.Cloud == nil || !a.Cloud.Missing)
		if upcoming {
			days := *view.DaysRemaining
			if days <= 7 {
				out.Expiring7++
			}
			if days <= 15 {
				out.Expiring15++
			}
			if days <= 30 {
				out.Expiring30++
			}
		}
		if filters.ExpiryDays != nil && (!upcoming || *view.DaysRemaining > *filters.ExpiryDays) {
			continue
		}
		out.Assets = append(out.Assets, view)
		out.Providers[a.Provider]++
		out.Regions[a.Region]++
	}
	slices.SortStableFunc(out.Assets, func(a, b model.AssetView) int {
		switch filters.Sort {
		case "expiry":
			if a.EffectiveExpiry == nil && b.EffectiveExpiry != nil {
				return 1
			}
			if a.EffectiveExpiry != nil && b.EffectiveExpiry == nil {
				return -1
			}
			if a.EffectiveExpiry != nil && b.EffectiveExpiry != nil {
				return a.EffectiveExpiry.Compare(*b.EffectiveExpiry)
			}
		case "updated":
			return b.UpdatedAt.Compare(a.UpdatedAt)
		}
		return strings.Compare(strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName))
	})
	out.Count = len(out.Assets)
	limit := filters.Limit
	if limit == 0 {
		limit = 50
	}
	start := min(filters.Offset, out.Count)
	out.Assets = out.Assets[start:min(start+limit, out.Count)]
	for i := range out.Assets {
		if out.Assets[i].Monitor == nil {
			out.Assets[i].Monitor = assetMonitoring(c, out.Assets[i].Asset, now, alerts)
		}
	}
	return out, nil
}

// @Summary List private assets with monitoring and expiry summaries
// @Tags Asset
// @Security APITokenAuth
// @Param q query string false "Text search"
// @Param provider query string false "Cloud provider"
// @Param region query string false "Region"
// @Param purpose query string false "Business purpose"
// @Param project query string false "Project"
// @Param group_id query int false "Server group ID"
// @Param expiry_days query int false "Expires within this many days"
// @Param issue query string false "offline, alert, expired, unmonitored"
// @Param sort query string false "name, expiry, updated"
// @Param offset query int false "Page offset"
// @Param limit query int false "Page size (max 200)"
// @Success 200 {object} model.CommonResponse[model.AssetList]
// @Router /asset [get]
func listAssets(c *gin.Context) (model.AssetList, error) {
	var f assetFilters
	if err := c.ShouldBindQuery(&f); err != nil {
		return model.AssetList{}, errors.New("invalid asset filters")
	}
	return buildAssetList(c, f)
}

// @Summary Get an asset and its Agent snapshot
// @Tags Asset
// @Security APITokenAuth
// @Param id path int true "Asset ID"
// @Success 200 {object} model.CommonResponse[model.AssetView]
// @Router /asset/{id} [get]
func getAsset(c *gin.Context) (model.AssetView, error) {
	id, err := assetID(c)
	if err != nil {
		return model.AssetView{}, err
	}
	return getAssetByID(c, id)
}

func getAssetByID(c *gin.Context, id uint64) (model.AssetView, error) {
	db := singleton.DB.WithContext(c.Request.Context())
	a, err := readAsset(c, db, id)
	if err != nil {
		return model.AssetView{}, err
	}
	parents := make(map[uint64]bool)
	if a.ParentID != nil {
		_, err := readAsset(c, db, *a.ParentID)
		parents[*a.ParentID] = err == nil
	}
	now := time.Now()
	view := assetViewFor(a, now, parents)
	view.Monitor = assetMonitoring(c, a, now, singleton.ActiveAlertCounts(getUid(c), callerIsAdmin(c)))
	return view, nil
}

func validateAssetRelations(c *gin.Context, db *gorm.DB, a *model.Asset, f *model.AssetFields) error {
	if f.ServerID == nil {
		if patHasServerWhitelist(c) {
			return model.ErrAssetNotFound
		}
	} else {
		s, allowed := assetServer(c, *f.ServerID)
		if !allowed {
			return errors.New("monitoring node not found or access denied")
		}
		if !callerIsAdmin(c) && s.GetUserID() != a.GetUserID() {
			return model.ErrAssetNotFound
		}
	}
	if f.GroupID != nil {
		var g model.ServerGroup
		if err := db.First(&g, *f.GroupID).Error; err != nil || !g.HasPermission(c) {
			return errors.New("server group not found or access denied")
		}
	}
	if f.ReminderGroup != nil {
		var g model.NotificationGroup
		if err := db.First(&g, *f.ReminderGroup).Error; err != nil || g.GetUserID() != a.GetUserID() {
			return errors.New("use a notification group owned by the asset owner")
		}
	}
	visited := make(map[uint64]bool)
	for parent := f.ParentID; parent != nil; {
		p, err := readAsset(c, db, *parent)
		if err != nil || p.GetUserID() != a.GetUserID() {
			return errors.New("parent asset not found or access denied")
		}
		if p.ID == a.ID || visited[p.ID] {
			return errors.New("asset hierarchy cannot contain a cycle")
		}
		visited[p.ID] = true
		parent = p.ParentID
	}
	return nil
}

func bindAssetFields(c *gin.Context, f *model.AssetFields) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024)
	if err := c.ShouldBindJSON(f); err != nil {
		return errors.New("invalid asset fields")
	}
	return nil
}

// @Summary Create a private asset
// @Tags Asset
// @Security APITokenAuth
// @Param body body model.AssetFields true "Asset fields"
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /asset [post]
func createAsset(c *gin.Context) (uint64, error) {
	a := model.Asset{Common: model.Common{UserID: getUid(c)}, Source: "manual"}
	if err := bindAssetFields(c, &a.AssetFields); err != nil {
		return 0, err
	}
	if err := assets.Validate(&a.AssetFields, false); err != nil {
		return 0, err
	}
	a.Identity = assets.Identity(a.Provider, a.Account, a.Region, a.InstanceID)
	err := singleton.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := validateAssetRelations(c, tx, &a, &a.AssetFields); err != nil {
			return err
		}
		if err := tx.Create(&a).Error; err != nil {
			return assetWriteError(err)
		}
		return nil
	})
	return a.ID, err
}

// @Summary Replace manual asset fields
// @Tags Asset
// @Security APITokenAuth
// @Param id path int true "Asset ID"
// @Param body body model.AssetFields true "Asset fields"
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /asset/{id} [put]
func updateAsset(c *gin.Context) (uint64, error) {
	id, err := assetID(c)
	if err != nil {
		return 0, err
	}
	var f model.AssetFields
	if err := bindAssetFields(c, &f); err != nil {
		return 0, err
	}
	err = singleton.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		a, err := readAsset(c, tx, id)
		if err != nil {
			return err
		}
		if err := assets.Validate(&f, a.Cloud != nil); err != nil {
			return err
		}
		identity := assets.Identity(f.Provider, f.Account, f.Region, f.InstanceID)
		if a.Cloud != nil && (identity == nil || a.Identity == nil || *identity != *a.Identity) {
			return errors.New("the identity of a synchronized asset is fixed")
		}
		if err := validateAssetRelations(c, tx, a, &f); err != nil {
			return err
		}
		next := model.Asset{AssetFields: f, Identity: identity}
		fields := []string{"Name", "Purpose", "Provider", "Account", "Region", "Zone", "InstanceID", "ServerID", "ParentID", "GroupID", "Project", "Environment", "Kind", "Status", "Tags", "Note",
			"PublicIPs", "PrivateIPs", "Domains", "Bandwidth", "Traffic", "CPUCores", "MemoryMiB", "StorageGiB", "OS", "Billing", "PurchasedAt", "ExpiresAt", "AutoRenew",
			"PurchasePrice", "RenewalPrice", "Currency", "Cycle", "CycleCount", "ReminderGroup", "ReminderDays", "Identity", "UpdatedAt"}
		if err := tx.Model(a).Select(fields).Updates(&next).Error; err != nil {
			return assetWriteError(err)
		}
		return nil
	})
	return id, err
}

func assetWriteError(err error) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return errors.New("asset, monitoring node or renewal already exists")
	}
	return newGormError("write asset: %v", err)
}

// @Summary Delete an asset record
// @Tags Asset
// @Security APITokenAuth
// @Param id path int true "Asset ID"
// @Success 200 {object} model.CommonResponse[any]
// @Router /asset/{id} [delete]
func deleteAsset(c *gin.Context) (any, error) {
	id, err := assetID(c)
	if err != nil {
		return nil, err
	}
	err = singleton.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		a, err := readAsset(c, tx, id)
		if err != nil {
			return err
		}
		var children int64
		if err := tx.Model(&model.Asset{}).Where("parent_id = ?", id).Count(&children).Error; err != nil {
			return assetWriteError(err)
		}
		if children != 0 {
			return errors.New("detach child assets before deleting this asset")
		}
		// Keep a provider tombstone so routine inventory sync does not recreate the row.
		if a.Cloud != nil {
			if err := tx.Model(a).Update("server_id", nil).Error; err != nil {
				return assetWriteError(err)
			}
			return assetWriteErrorOrNil(tx.Delete(a).Error)
		}
		for _, row := range []any{&model.AssetRenewal{}, &model.AssetBill{}, &model.AssetReminder{}} {
			if err := tx.Where("asset_id = ?", id).Delete(row).Error; err != nil {
				return assetWriteError(err)
			}
		}
		return assetWriteErrorOrNil(tx.Unscoped().Delete(a).Error)
	})
	return nil, err
}

func assetWriteErrorOrNil(err error) error {
	if err != nil {
		return assetWriteError(err)
	}
	return nil
}

// @Summary Import supported legacy public note fields
// @Tags Asset
// @Security APITokenAuth
// @Param id path int true "Agent server ID"
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /asset/import/{id} [post]
func importAsset(c *gin.Context) (uint64, error) {
	serverID, err := assetID(c)
	if err != nil {
		return 0, err
	}
	server, allowed := assetServer(c, serverID)
	if !allowed {
		return 0, model.ErrAssetNotFound
	}
	owner := server.GetUserID()
	if owner == 0 {
		owner = getUid(c)
	}
	a, err := assets.FromPublicNote(server.Name, server.PublicNote, owner, server.ID)
	if err != nil {
		return 0, err
	}
	err = singleton.DB.WithContext(c.Request.Context()).Create(a).Error
	return a.ID, assetWriteErrorOrNil(err)
}
