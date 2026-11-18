package singleton

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nezhahq/nezha/model"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestAssetRemindersRetryAndFollowActualExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reminders.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Asset{}, &model.AssetReminder{}, &model.NotificationGroup{}))
	group := model.NotificationGroup{Common: model.Common{UserID: 5}, Name: "renewals"}
	require.NoError(t, db.Create(&group).Error)
	now := time.Now().UTC().Truncate(time.Second)
	expiry := now.Add(6 * 24 * time.Hour)
	newExpiry := expiry.Add(30 * 24 * time.Hour)
	enabled := true
	a := model.Asset{Common: model.Common{UserID: 5}, AssetFields: model.AssetFields{Name: "ECS", ExpiresAt: &newExpiry, ReminderGroup: &group.ID, ReminderDays: []int{7, 15, 30}},
		Cloud: &model.AssetCloud{Billing: "subscription", AutoRenew: &enabled, ExpiresAt: &expiry}}
	require.NoError(t, db.Create(&a).Error)
	calls := 0
	send := func(id uint64, msg, label string) bool {
		calls++
		require.Equal(t, group.ID, id)
		require.Contains(t, msg, "ECS")
		require.NotEmpty(t, label)
		return calls != 1
	}
	require.NoError(t, sendAssetReminders(db, now, send))
	require.NoError(t, sendAssetReminders(db, now.Add(time.Hour), send))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	db, err = gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, sendAssetReminders(db, now.Add(time.Hour), send))
	require.Equal(t, 2, calls, "successful reminders must survive restart")
	a.Cloud.ExpiresAt = &newExpiry
	require.NoError(t, db.Model(&a).Select("Cloud").Updates(&a).Error)
	require.NoError(t, sendAssetReminders(db, now.Add(7*24*time.Hour), send))
	require.Equal(t, 3, calls, "provider renewal starts a new reminder cycle")
}
