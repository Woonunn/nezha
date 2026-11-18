package controller

import (
	"errors"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func cloudAllowed(c *gin.Context) bool {
	return callerIsAdmin(c) && !patHasServerWhitelist(c)
}

// @Summary List cloud account sync status without credentials
// @Tags Asset
// @Security APITokenAuth
// @Success 200 {object} model.CommonResponse[model.AssetCloudStatusView]
// @Router /asset/cloud [get]
func assetCloudStatus(c *gin.Context) (model.AssetCloudStatusView, error) {
	out := model.AssetCloudStatusView{Accounts: make([]model.CloudAccountView, 0), ConfigError: singleton.AssetCloudConfigError}
	if !cloudAllowed(c) {
		return out, errMCPPermDenied
	}
	if singleton.AssetCloud != nil {
		accounts, err := singleton.AssetCloud.Status(c.Request.Context())
		if err != nil {
			return out, newGormError("load cloud status: %v", err)
		}
		out.Accounts = accounts
	}
	return out, nil
}

// @Summary Start an asynchronous read-only cloud sync
// @Tags Asset
// @Security APITokenAuth
// @Param provider path string true "Provider identifier"
// @Param account path string true "Account identifier"
// @Success 200 {object} model.CommonResponse[model.AssetCloudSyncResult]
// @Router /asset/cloud/{provider}/{account}/sync [post]
func triggerAssetCloudSync(c *gin.Context) (model.AssetCloudSyncResult, error) {
	return startAssetCloudSync(c, c.Param("provider"), c.Param("account"))
}

func startAssetCloudSync(c *gin.Context, provider, account string) (model.AssetCloudSyncResult, error) {
	if !cloudAllowed(c) {
		return model.AssetCloudSyncResult{}, errMCPPermDenied
	}
	if provider == "" || account == "" {
		return model.AssetCloudSyncResult{}, errors.New("provider and account are required")
	}
	if singleton.AssetCloud == nil {
		return model.AssetCloudSyncResult{}, errors.New("cloud sync is not configured")
	}
	started, err := singleton.AssetCloud.Start(provider, account)
	return model.AssetCloudSyncResult{Started: started, RequestedAt: time.Now().UTC()}, err
}
