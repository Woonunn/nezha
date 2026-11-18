package controller

import (
	"encoding/json"
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/nezhahq/nezha/model"
)

func assetMCPHandler[T any](fn func(*gin.Context, json.RawMessage) (T, error)) mcpToolHandler {
	return func(c *gin.Context, raw json.RawMessage) (any, error) {
		result, err := fn(c, raw)
		if err == nil {
			return result, nil
		}
		var dbErr *gormError
		if errors.As(err, &dbErr) {
			return nil, newMCPError(model.MCPOutcomeAgentError, "database error")
		}
		if err == errMCPPermDenied || errors.Is(err, model.ErrAssetNotFound) {
			return nil, errMCPPermDenied
		}
		var toolErr *mcpError
		if errors.As(err, &toolErr) {
			return nil, toolErr
		}
		return nil, errMCPInvalidArgs(err.Error())
	}
}

type mcpAssetCosts struct {
	Costs []model.AssetCostRow `json:"costs"`
}

func mcpAssetSchema(properties map[string]any, required ...string) map[string]any {
	if properties == nil {
		properties = make(map[string]any)
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func init() {
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	integer := func(description string, min, max int) map[string]any {
		return map[string]any{"type": "integer", "description": description, "minimum": min, "maximum": max}
	}
	list := map[string]any{
		"q":           str("Search names, IP addresses, purpose, region, instance ID or tags."),
		"provider":    str("Exact cloud provider, e.g. aliyun."),
		"region":      str("Exact provider region."),
		"purpose":     str("Exact business purpose."),
		"project":     str("Exact project."),
		"status":      str("Exact asset or cloud runtime status."),
		"billing":     str("subscription, on_demand, permanent or unknown."),
		"tag":         str("Exact asset tag."),
		"group_id":    map[string]any{"type": "integer", "minimum": 1},
		"server_id":   map[string]any{"type": "integer", "minimum": 1},
		"expiry_days": integer("Upcoming expiry window in days.", 0, 3650),
		"issue":       str("offline, alert, expired or unmonitored."),
		"sort":        str("name, expiry or updated."),
		"limit":       integer("Page size (default 50).", 1, 200),
		"offset":      integer("Pagination offset.", 0, 1_000_000_000),
	}
	registerMCPTool(&mcpTool{Name: "asset.list", Description: "List accessible private server assets, including unmonitored assets, live CPU, RAM, disk and network, expiry and active alert counts.",
		RequiredScope: model.ScopeAssetRead, InputSchema: mcpAssetSchema(list), Handler: assetMCPHandler(func(c *gin.Context, raw json.RawMessage) (model.AssetList, error) {
			var f assetFilters
			if err := decodeToolArgs(raw, &f); err != nil {
				return model.AssetList{}, err
			}
			return buildAssetList(c, f)
		})})
	registerMCPTool(&mcpTool{Name: "asset.get", Description: "Get a private asset and the latest status of its linked Agent by asset ID.",
		RequiredScope: model.ScopeAssetRead, InputSchema: mcpAssetSchema(map[string]any{"asset_id": map[string]any{"type": "integer", "minimum": 1}}, "asset_id"), Handler: assetMCPHandler(func(c *gin.Context, raw json.RawMessage) (model.AssetView, error) {
			var args struct {
				AssetID uint64 `json:"asset_id"`
			}
			if err := decodeToolArgs(raw, &args); err != nil {
				return model.AssetView{}, err
			}
			if args.AssetID == 0 {
				return model.AssetView{}, errors.New("asset_id is required")
			}
			return getAssetByID(c, args.AssetID)
		})})
	registerMCPTool(&mcpTool{Name: "asset.expiring", Description: "List assets that expire within the given number of days, earliest first.",
		RequiredScope: model.ScopeAssetRead, InputSchema: mcpAssetSchema(map[string]any{
			"days": integer("Upcoming expiry window.", 0, 3650), "limit": list["limit"], "offset": list["offset"],
		}, "days"), Handler: assetMCPHandler(func(c *gin.Context, raw json.RawMessage) (model.AssetList, error) {
			var args struct {
				Days   *int `json:"days"`
				Limit  int  `json:"limit"`
				Offset int  `json:"offset"`
			}
			if err := decodeToolArgs(raw, &args); err != nil {
				return model.AssetList{}, err
			}
			if args.Days == nil {
				return model.AssetList{}, errors.New("days is required")
			}
			return buildAssetList(c, assetFilters{ExpiryDays: args.Days, Sort: "expiry", Limit: args.Limit, Offset: args.Offset})
		})})
	registerMCPTool(&mcpTool{Name: "asset.costs", Description: "Summarize authorized asset charges by provider, project and currency. Cloud billed, manual and estimated figures are separate.",
		RequiredScope: model.ScopeAssetRead, InputSchema: mcpAssetSchema(map[string]any{
			"period": str("YYYY or YYYY-MM (default current month)."), "provider": list["provider"], "project": list["project"],
		}), Handler: assetMCPHandler(func(c *gin.Context, raw json.RawMessage) (mcpAssetCosts, error) {
			var filters assetCostFilters
			if err := decodeToolArgs(raw, &filters); err != nil {
				return mcpAssetCosts{}, err
			}
			costs, err := costReport(c, filters)
			return mcpAssetCosts{Costs: costs}, err
		})})
	registerMCPTool(&mcpTool{Name: "cloud.status", Description: "Get configured cloud account sync status. Requires an administrator and an account-wide token.",
		RequiredScope: model.ScopeCloudRead, InputSchema: mcpAssetSchema(nil), Handler: assetMCPHandler(func(c *gin.Context, _ json.RawMessage) (model.AssetCloudStatusView, error) {
			return assetCloudStatus(c)
		})})
	registerMCPTool(&mcpTool{Name: "cloud.sync", Description: "Start an asynchronous cloud account sync using read-only cloud APIs. Requires explicit sync permission and an account-wide administrator token.",
		RequiredScope: model.ScopeCloudSync, InputSchema: mcpAssetSchema(map[string]any{"provider": list["provider"], "account": str("Configured account ID.")}, "provider", "account"),
		Handler: assetMCPHandler(func(c *gin.Context, raw json.RawMessage) (model.AssetCloudSyncResult, error) {
			var args struct {
				Provider string `json:"provider"`
				Account  string `json:"account"`
			}
			if err := decodeToolArgs(raw, &args); err != nil {
				return model.AssetCloudSyncResult{}, err
			}
			return startAssetCloudSync(c, args.Provider, args.Account)
		})})
}
