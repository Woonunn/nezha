package singleton

import (
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/asset"
)

var (
	assetReminderRun      sync.Mutex
	AssetCloud            *asset.CloudManager
	AssetCloudConfigError string
)

func SendAssetExpiryReminders() {
	if !assetReminderRun.TryLock() {
		return
	}
	defer assetReminderRun.Unlock()
	if err := sendAssetReminders(DB, time.Now(), func(group uint64, message, key string) bool {
		return NotificationShared.SendNotification(group, message, key)
	}); err != nil {
		log.Printf("NEZHA>> Asset expiry reminders: %v", err)
	}
}

func sendAssetReminders(db *gorm.DB, now time.Time, send func(uint64, string, string) bool) error {
	var rows []model.Asset
	if err := db.Where("reminder_group IS NOT NULL").Find(&rows).Error; err != nil {
		return err
	}
	for _, a := range rows {
		expires := a.EffectiveExpiry()
		if expires == nil || a.Status == "retired" || (a.Cloud != nil && a.Cloud.Missing) {
			continue
		}
		// An hourly retry can still deliver the day-zero reminder after an outage.
		days := asset.DaysRemaining(*expires, now)
		if days < -1 || len(a.ReminderDays) == 0 {
			continue
		}
		group := *a.ReminderGroup
		var notifier model.NotificationGroup
		if err := db.Where("id = ? AND user_id = ?", group, a.UserID).Find(&notifier).Error; err != nil {
			return err
		}
		if notifier.ID == 0 {
			continue
		}
		var sentDays []int
		if err := db.Model(&model.AssetReminder{}).Where("asset_id = ? AND group_id = ? AND expires_at = ?", a.ID, group, expires.UTC()).Pluck("days", &sentDays).Error; err != nil {
			return err
		}
		var pending []model.AssetReminder
		for _, threshold := range a.ReminderDays {
			if days <= threshold && !slices.Contains(sentDays, threshold) {
				pending = append(pending, model.AssetReminder{AssetID: a.ID, GroupID: group, ExpiresAt: expires.UTC(), Days: threshold, SentAt: now.UTC()})
			}
		}
		if len(pending) == 0 {
			continue
		}
		label := fmt.Sprintf("asset-expiry-%d-%d-%s-%d", a.ID, group, expires.UTC().Format(time.RFC3339Nano), pending[0].Days)
		loc := now.Location()
		if Loc != nil {
			loc = Loc
		}
		message := fmt.Sprintf("[Asset expiry] %s (#%d) · %s · %d day(s) remaining", a.DisplayName(), a.ID, expires.In(loc).Format(time.RFC3339), days)
		if send(group, message, label) {
			if err := db.Create(&pending).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
