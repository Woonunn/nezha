package asset

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nezhahq/nezha/model"
)

const NoExpiry = "0000-00-00T23:59:59+08:00"

var (
	moneyPattern    = regexp.MustCompile(`^-?[0-9]{1,12}(\.[0-9]{1,6})?$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	legacyPrice     = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*([A-Z]{3})$`)
)

func Identity(provider, account, region, instance string) *string {
	if provider == "" || account == "" || region == "" || instance == "" {
		return nil
	}
	value, _ := json.Marshal([4]string{provider, account, region, instance})
	key := string(value)
	return &key
}

func Micros(amount string) (int64, error) {
	if !moneyPattern.MatchString(amount) {
		return 0, errors.New("amount must be a decimal with up to six fractional digits")
	}
	value, _ := new(big.Rat).SetString(amount)
	return value.Mul(value, big.NewRat(1_000_000, 1)).Num().Int64(), nil
}

func ValidateMoney(amount, currency string) error {
	n, err := Micros(amount)
	if err != nil || n < 0 || !currencyPattern.MatchString(currency) {
		return errors.New("enter a nonnegative decimal amount and a three-letter currency code")
	}
	return nil
}

func Validate(fields *model.AssetFields, synced bool) error {
	for _, s := range []*string{&fields.Name, &fields.Purpose, &fields.Provider, &fields.Account, &fields.Region, &fields.Zone, &fields.InstanceID, &fields.Project, &fields.Environment, &fields.Kind, &fields.Status, &fields.PublicIPs, &fields.PrivateIPs, &fields.Domains, &fields.Bandwidth, &fields.Traffic, &fields.OS} {
		*s = strings.TrimSpace(*s)
		if len(*s) > 1024 {
			return errors.New("asset field is too long")
		}
	}
	fields.Provider = strings.ToLower(fields.Provider)
	fields.Currency = strings.ToUpper(strings.TrimSpace(fields.Currency))
	if (fields.Name == "" && !synced) || len(fields.Note) > 16_384 || len(fields.Tags) > 50 {
		return errors.New("asset requires a name and has limits of 16 KB for notes and 50 tags")
	}
	for i, tag := range fields.Tags {
		fields.Tags[i] = strings.TrimSpace(tag)
		if fields.Tags[i] == "" || len(fields.Tags[i]) > 128 {
			return errors.New("tags must contain 1–128 bytes")
		}
	}
	if fields.Billing == "" {
		fields.Billing = "unknown"
	}
	if !slices.Contains([]string{"subscription", "on_demand", "permanent", "unknown"}, fields.Billing) {
		return errors.New("invalid billing mode")
	}
	if fields.Kind != "" && !slices.Contains([]string{"physical", "esxi", "vm", "cloud", "other"}, fields.Kind) {
		return errors.New("invalid asset kind")
	}
	if fields.CPUCores < 0 || fields.MemoryMiB < 0 || fields.StorageGiB < 0 {
		return errors.New("hardware capacity cannot be negative")
	}
	if fields.Cycle == "" {
		fields.CycleCount = 0
	} else if !slices.Contains([]string{"day", "week", "month", "year"}, fields.Cycle) || fields.CycleCount < 1 || fields.CycleCount > 366 {
		return errors.New("invalid billing cycle")
	}
	if fields.Currency != "" && !currencyPattern.MatchString(fields.Currency) {
		return errors.New("currency must be a three-letter code")
	}
	for _, amount := range []string{fields.PurchasePrice, fields.RenewalPrice} {
		if amount != "" {
			if err := ValidateMoney(amount, fields.Currency); err != nil {
				return err
			}
		}
	}
	for _, date := range []**time.Time{&fields.PurchasedAt, &fields.ExpiresAt} {
		if *date != nil {
			if (*date).Year() < 1 || (*date).Year() > 9999 {
				return errors.New("invalid date")
			}
			utc := (*date).UTC()
			*date = &utc
		}
	}
	if len(fields.ReminderDays) > 20 {
		return errors.New("at most 20 reminder thresholds are allowed")
	}
	for _, days := range fields.ReminderDays {
		if days < 0 || days > 365 {
			return errors.New("reminder days must be between 0 and 365")
		}
	}
	if len(fields.ReminderDays) > 0 && fields.ReminderGroup == nil {
		return errors.New("choose a notification group for expiry reminders")
	}
	slices.Sort(fields.ReminderDays)
	fields.ReminderDays = slices.Compact(fields.ReminderDays)
	return nil
}

func FromPublicNote(name, note string, owner, serverID uint64) (*model.Asset, error) {
	var payload struct {
		Billing struct {
			StartDate, EndDate, AutoRenewal, Cycle, Amount string
		} `json:"billingDataMod"`
		Plan struct {
			Bandwidth, TrafficVol string
		} `json:"planDataMod"`
	}
	if err := json.Unmarshal([]byte(note), &payload); err != nil {
		return nil, errors.New("server has no structured public note to import")
	}
	b := payload.Billing
	if b.StartDate == "" && b.EndDate == "" && b.AutoRenewal == "" && b.Cycle == "" && b.Amount == "" && payload.Plan.Bandwidth == "" && payload.Plan.TrafficVol == "" {
		return nil, errors.New("server has no supported billing or plan fields to import")
	}
	a := &model.Asset{Common: model.Common{UserID: owner}, Source: "legacy_public_note", LegacyPrice: b.Amount}
	a.AssetFields = model.AssetFields{Name: name, ServerID: &serverID, Billing: "unknown"}
	a.Bandwidth, a.Traffic = payload.Plan.Bandwidth, payload.Plan.TrafficVol
	a.PurchasedAt, a.ExpiresAt = ParseDate(b.StartDate), ParseDate(b.EndDate)
	switch {
	case b.EndDate == NoExpiry:
		a.Billing = "permanent"
	case b.Amount == "-1":
		a.Billing = "on_demand"
	case a.ExpiresAt != nil:
		a.Billing = "subscription"
	}
	if b.AutoRenewal == "1" || b.AutoRenewal == "0" {
		enabled := b.AutoRenewal == "1"
		a.AutoRenew = &enabled
	}
	if cycle := strings.ToLower(b.Cycle); slices.Contains([]string{"day", "week", "month", "year"}, cycle) {
		a.Cycle, a.CycleCount = cycle, 1
	}
	if match := legacyPrice.FindStringSubmatch(strings.TrimSpace(b.Amount)); match != nil {
		currency := strings.ToUpper(match[2])
		if ValidateMoney(match[1], currency) == nil {
			a.PurchasePrice, a.Currency = match[1], currency
		}
	}
	return a, Validate(&a.AssetFields, false)
}

func ParseDate(raw string) *time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z", "2006-01-02"} {
		if date, err := time.Parse(layout, raw); err == nil && date.Year() >= 1 {
			utc := date.UTC()
			return &utc
		}
	}
	return nil
}

func DaysRemaining(expiry, now time.Time) int {
	// Round positive partial days up while leaving an expired interval negative.
	delta := expiry.Sub(now)
	const day = 24 * time.Hour
	whole := int(delta / day)
	if delta%day > 0 {
		whole++
	} else if delta%day < 0 {
		whole--
	}
	return whole
}

func CycleYearFactor(unit string, count int) *big.Rat {
	if count <= 0 {
		return nil
	}
	switch unit {
	case "day":
		return big.NewRat(365, int64(count))
	case "week":
		return big.NewRat(365, int64(count)*7)
	case "month":
		return big.NewRat(12, int64(count))
	case "year":
		return big.NewRat(1, int64(count))
	}
	return nil
}

func ParsePeriod(period string) (time.Time, time.Time, error) {
	for _, layout := range []string{"2006-01", "2006"} {
		start, err := time.Parse(layout, period)
		if err == nil && start.Year() >= 1 && start.Year() < 9999 {
			if layout == "2006-01" {
				return start, start.AddDate(0, 1, 0), nil
			}
			return start, start.AddDate(1, 0, 0), nil
		}
	}
	return time.Time{}, time.Time{}, fmt.Errorf("period must be YYYY or YYYY-MM")
}
