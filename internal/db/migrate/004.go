package migrate

import (
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 4,
		Up:      seedDefaultAccessPlans,
	})
}

func seedDefaultAccessPlans(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	defaults := []struct {
		Slug        string
		DisplayName string
		Sort        int
		IsDefault   bool
	}{
		{Slug: "vip", DisplayName: "VIP", Sort: 10, IsDefault: true},
		{Slug: "svip", DisplayName: "SVIP", Sort: 20},
		{Slug: "ssvip", DisplayName: "SSVIP", Sort: 30},
	}

	for _, item := range defaults {
		routeProfile, err := accessRouteProfileByName(db, item.Slug+" route profile")
		if err != nil {
			return err
		}
		billingProfile, err := accessBillingProfileByName(db, item.Slug+" billing profile")
		if err != nil {
			return err
		}

		var existing model.AccessPlan
		err = db.Where("slug = ?", item.Slug).First(&existing).Error
		if err == nil {
			continue
		}
		if err != nil && err != gorm.ErrRecordNotFound {
			return err
		}

		plan := model.AccessPlan{
			Slug:             item.Slug,
			DisplayName:      item.DisplayName,
			Sort:             item.Sort,
			Enabled:          true,
			IsDefault:        item.IsDefault,
			RouteProfileID:   routeProfile.ID,
			BillingProfileID: billingProfile.ID,
		}
		if err := db.Create(&plan).Error; err != nil {
			return err
		}
	}

	var defaultCount int64
	if err := db.Model(&model.AccessPlan{}).Where("enabled = ? AND is_default = ?", true, true).Count(&defaultCount).Error; err != nil {
		return err
	}
	if defaultCount == 0 {
		if err := db.Model(&model.AccessPlan{}).Where("slug = ?", "vip").Update("is_default", true).Error; err != nil {
			return err
		}
	}

	return backfillAPIKeyAccessPlanDefaults(db)
}

func accessRouteProfileByName(db *gorm.DB, name string) (model.AccessRouteProfile, error) {
	var profile model.AccessRouteProfile
	err := db.Where("name = ?", name).First(&profile).Error
	if err == nil {
		return profile, nil
	}
	if err != nil && err != gorm.ErrRecordNotFound {
		return model.AccessRouteProfile{}, err
	}
	profile = model.AccessRouteProfile{Name: name}
	if err := db.Create(&profile).Error; err != nil {
		return model.AccessRouteProfile{}, err
	}
	return profile, nil
}

func accessBillingProfileByName(db *gorm.DB, name string) (model.AccessBillingProfile, error) {
	var profile model.AccessBillingProfile
	err := db.Where("name = ?", name).First(&profile).Error
	if err == nil {
		if profile.DefaultMultiplier <= 0 {
			profile.DefaultMultiplier = 1
			if err := db.Save(&profile).Error; err != nil {
				return model.AccessBillingProfile{}, err
			}
		}
		return profile, nil
	}
	if err != nil && err != gorm.ErrRecordNotFound {
		return model.AccessBillingProfile{}, err
	}
	profile = model.AccessBillingProfile{Name: name, DefaultMultiplier: 1}
	if err := db.Create(&profile).Error; err != nil {
		return model.AccessBillingProfile{}, err
	}
	return profile, nil
}

func backfillAPIKeyAccessPlanDefaults(db *gorm.DB) error {
	var defaultPlan model.AccessPlan
	err := db.Where("enabled = ? AND is_default = ?", true, true).Order("sort ASC, id ASC").First(&defaultPlan).Error
	if err != nil {
		return nil
	}

	var apiKeys []model.APIKey
	if err := db.Find(&apiKeys).Error; err != nil {
		return err
	}
	for _, apiKey := range apiKeys {
		var count int64
		if err := db.Model(&model.APIKeyAccessPlan{}).Where("api_key_id = ?", apiKey.ID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		row := model.APIKeyAccessPlan{
			APIKeyID:     apiKey.ID,
			AccessPlanID: defaultPlan.ID,
			IsDefault:    true,
		}
		if err := db.Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}
