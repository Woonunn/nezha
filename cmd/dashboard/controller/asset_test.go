package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func TestAssetMigrationAndScopedAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	cleanup, uid := setupMCPTestWithDatabase(t, path)
	defer cleanup()
	legacy := model.Server{Common: model.Common{UserID: uid}, Name: "legacy host", UUID: "legacy-id",
		PublicNote: `{"billingDataMod":{"amount":"20EUR","cycle":"Month","endDate":"2027-01-01"}}`}
	require.NoError(t, singleton.DB.Create(&legacy).Error)
	require.NoError(t, singleton.InitDBFromPath(path))
	db := singleton.DB
	t.Cleanup(func() { sql, _ := db.DB(); sql.Close() })
	var old model.Server
	require.NoError(t, db.First(&old, legacy.ID).Error)
	require.Equal(t, legacy.PublicNote, old.PublicNote)
	require.Equal(t, legacy.UUID, old.UUID)

	foreign := &model.Server{Common: model.Common{ID: 8, UserID: 200}, Name: "foreign node"}
	singleton.ServerShared.InsertForTest(foreign)
	localServer := uint64(7)
	foreignServer := foreign.ID
	expiration := time.Now().UTC().Add(4 * 24 * time.Hour)
	purchased := time.Now().UTC().Add(-time.Hour)
	linked := model.Asset{Common: model.Common{UserID: uid}, AssetFields: model.AssetFields{Name: "linked", ServerID: &localServer, ExpiresAt: &expiration,
		Provider: "lcayun", Project: "core", Billing: "subscription", PurchasedAt: &purchased, PurchasePrice: "12.50", RenewalPrice: "10", Currency: "CNY", Cycle: "month", CycleCount: 1}}
	unlinked := model.Asset{Common: model.Common{UserID: uid}, AssetFields: model.AssetFields{Name: "unlinked", Provider: "lcayun", Project: "core",
		PurchasedAt: &purchased, PurchasePrice: "30", Currency: "USD"}}
	foreignLink := model.Asset{Common: model.Common{UserID: uid}, AssetFields: model.AssetFields{Name: "transferred node", ServerID: &foreignServer}}
	foreignOwner := model.Asset{Common: model.Common{UserID: 200}, AssetFields: model.AssetFields{Name: "foreign asset"}}
	for _, a := range []*model.Asset{&linked, &unlinked, &foreignLink, &foreignOwner} {
		require.NoError(t, db.Create(a).Error)
	}
	tok, _ := mkToken(t, uid, []string{model.ScopeAssetRead}, []uint64{localServer})
	c, _ := mcpCallCtx(t, tok, uid, nil)
	rows, err := buildAssetList(c, assetFilters{})
	require.NoError(t, err)
	require.Len(t, rows.Assets, 1)
	require.Equal(t, linked.ID, rows.Assets[0].ID)
	require.EqualValues(t, 1, rows.Expiring7)
	require.NotNil(t, rows.Assets[0].Monitor)
	_, err = readAsset(c, db, unlinked.ID)
	require.ErrorIs(t, err, model.ErrAssetNotFound)
	costs, err := costReport(c, assetCostFilters{Period: purchased.Format("2006")})
	require.NoError(t, err)
	require.Len(t, costs, 1)
	require.Equal(t, "12.5", costs[0].ManualPurchase)
	require.Equal(t, "120", costs[0].EstimatedAnnual)

	tok.SetServerIDs(nil)
	rows, err = buildAssetList(c, assetFilters{})
	require.NoError(t, err)
	require.EqualValues(t, 2, rows.Count)
	costs, err = costReport(c, assetCostFilters{Period: purchased.Format("2006-01")})
	require.NoError(t, err)
	require.Len(t, costs, 2)
	require.Equal(t, "USD", costs[1].Currency)
	require.Equal(t, "30", costs[1].ManualPurchase)

	tok.SetServerIDs([]uint64{localServer})
	request := func(method, name string, args any) *mcpToolCallResult {
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]any{"name": name, "arguments": args}}
		ctx, w := mcpCallCtx(t, tok, uid, body)
		mcpEndpoint(ctx)
		_, reply := decodeRPC(w)
		require.NotNil(t, reply)
		return reply
	}
	expiring := request("tools/call", "asset.expiring", map[string]any{"days": 7})
	require.False(t, expiring.IsError)
	require.Contains(t, expiring.Content[0].Text, "linked")
	require.NotContains(t, expiring.Content[0].Text, "unlinked")
	require.False(t, request("tools/call", "asset.costs", map[string]any{}).IsError)
	require.True(t, request("tools/call", "cloud.sync", map[string]any{"provider": "aliyun", "account": "1234567890123456"}).IsError)
	require.True(t, request("tools/call", "server.exec", map[string]any{"server_id": 7, "command": "true"}).IsError)

	var membership model.User
	require.NoError(t, db.First(&membership, uid).Error)
	w := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/asset/cloud", nil)
	c.Set(model.CtxKeyAuthorizedUser, &membership)
	_, err = assetCloudStatus(c)
	require.ErrorIs(t, err, errMCPPermDenied)
	membership.Role = model.RoleAdmin
	c.Set(model.CtxKeyAPIToken, tok)
	_, err = assetCloudStatus(c)
	require.ErrorIs(t, err, errMCPPermDenied)
}

func TestAssetWriteAndRenewalPreserveCloudSnapshot(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	db := singleton.DB
	require.NoError(t, db.AutoMigrate(&model.Asset{}, &model.AssetRenewal{}, &model.AssetBill{}, &model.AssetReminder{}, &model.NotificationGroup{}, &model.ServerGroup{}))
	request := func(method string, id uint64, body any) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		data, err := json.Marshal(body)
		require.NoError(t, err)
		c.Request = httptest.NewRequest(method, "/api/v1/asset", bytes.NewReader(data))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set(model.CtxKeyAuthorizedUser, &model.User{Common: model.Common{ID: uid}, Role: model.RoleMember})
		c.Params = []gin.Param{{Key: "id", Value: fmt.Sprint(id)}}
		return c
	}
	f := model.AssetFields{Name: "VM", Kind: "vm", Billing: "subscription", Note: "private"}
	id, err := createAsset(request("POST", 0, f))
	require.NoError(t, err)
	require.Positive(t, id)
	child, err := createAsset(request("POST", 0, model.AssetFields{Name: "Child", ParentID: &id}))
	require.NoError(t, err)
	f.ParentID = &child
	_, err = updateAsset(request("PUT", id, f))
	require.ErrorContains(t, err, "cycle")
	f.ParentID = nil
	f.Note = ""
	_, err = updateAsset(request("PUT", id, f))
	require.NoError(t, err)
	payment := time.Now().UTC().Add(-time.Minute)
	next := payment.Add(7 * 24 * time.Hour)
	form := model.AssetRenewalForm{PaidAt: payment, ExpiresAt: next, Currency: "cny", Amount: "12.01"}
	first, err := addAssetRenewal(request("POST", id, form))
	require.NoError(t, err)
	second, err := addAssetRenewal(request("POST", id, form))
	require.NoError(t, err)
	require.Equal(t, first, second)
	history, err := assetHistory(request("GET", id, nil))
	require.NoError(t, err)
	require.Len(t, history.Renewals, 1)
	require.Equal(t, "CNY", history.Renewals[0].Currency)
	out, err := getAsset(request("GET", id, nil))
	require.NoError(t, err)
	require.Empty(t, out.Note)
	require.True(t, next.Equal(*out.EffectiveExpiry))
	_, err = deleteAsset(request("DELETE", id, nil))
	require.ErrorContains(t, err, "child assets")
	_, err = deleteAsset(request("DELETE", child, nil))
	require.NoError(t, err)
	_, err = deleteAsset(request("DELETE", id, nil))
	require.NoError(t, err)
	_, err = getAsset(request("GET", id, nil))
	require.ErrorIs(t, err, model.ErrAssetNotFound)
}
