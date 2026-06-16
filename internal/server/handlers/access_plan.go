package handlers

import (
	"net/http"
	"strconv"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

func init() {
	router.NewGroupRouter("/api/v1/access-plan").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(router.NewRoute("/list", http.MethodGet).Handle(listAccessPlans)).
		AddRoute(router.NewRoute("/route-profile/list", http.MethodGet).Handle(listAccessRouteProfiles)).
		AddRoute(router.NewRoute("/billing-profile/list", http.MethodGet).Handle(listAccessBillingProfiles)).
		AddRoute(router.NewRoute("/create", http.MethodPost).Handle(createAccessPlan)).
		AddRoute(router.NewRoute("/update", http.MethodPost).Handle(updateAccessPlan)).
		AddRoute(router.NewRoute("/delete/:id", http.MethodDelete).Handle(deleteAccessPlan)).
		AddRoute(router.NewRoute("/route-profile/create", http.MethodPost).Handle(createAccessRouteProfile)).
		AddRoute(router.NewRoute("/route-profile/update", http.MethodPost).Handle(updateAccessRouteProfile)).
		AddRoute(router.NewRoute("/route-profile/delete/:id", http.MethodDelete).Handle(deleteAccessRouteProfile)).
		AddRoute(router.NewRoute("/route-rule/create", http.MethodPost).Handle(createAccessRouteRule)).
		AddRoute(router.NewRoute("/route-rule/update", http.MethodPost).Handle(updateAccessRouteRule)).
		AddRoute(router.NewRoute("/route-rule/delete/:id", http.MethodDelete).Handle(deleteAccessRouteRule)).
		AddRoute(router.NewRoute("/route-target/create", http.MethodPost).Handle(createAccessRouteTarget)).
		AddRoute(router.NewRoute("/route-target/update", http.MethodPost).Handle(updateAccessRouteTarget)).
		AddRoute(router.NewRoute("/route-target/delete/:id", http.MethodDelete).Handle(deleteAccessRouteTarget)).
		AddRoute(router.NewRoute("/billing-profile/create", http.MethodPost).Handle(createAccessBillingProfile)).
		AddRoute(router.NewRoute("/billing-profile/update", http.MethodPost).Handle(updateAccessBillingProfile)).
		AddRoute(router.NewRoute("/billing-profile/delete/:id", http.MethodDelete).Handle(deleteAccessBillingProfile)).
		AddRoute(router.NewRoute("/billing-rule/update", http.MethodPost).Handle(updateAccessBillingRules)).
		AddRoute(router.NewRoute("/billing-model-rule/create", http.MethodPost).Handle(createAccessBillingModelRule)).
		AddRoute(router.NewRoute("/billing-model-rule/update", http.MethodPost).Handle(updateAccessBillingModelRule)).
		AddRoute(router.NewRoute("/billing-model-rule/delete/:id", http.MethodDelete).Handle(deleteAccessBillingModelRule)).
		AddRoute(router.NewRoute("/apikey/:id/bind", http.MethodPost).Handle(bindAPIKeyAccessPlans))
}

func listAccessPlans(c *gin.Context) {
	plans, err := op.AccessPlanList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, plans)
}

func listAccessRouteProfiles(c *gin.Context) {
	profiles, err := op.AccessRouteProfileList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, profiles)
}

func listAccessBillingProfiles(c *gin.Context) {
	profiles, err := op.AccessBillingProfileList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, profiles)
}

func createAccessPlan(c *gin.Context) {
	var plan model.AccessPlan
	if err := c.ShouldBindJSON(&plan); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.AccessPlanCreate(&plan, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, plan)
}

func updateAccessPlan(c *gin.Context) {
	var plan model.AccessPlan
	if err := c.ShouldBindJSON(&plan); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.AccessPlanUpdate(&plan, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, plan)
}

func deleteAccessPlan(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.AccessPlanDelete(id, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func createAccessRouteProfile(c *gin.Context) {
	var profile model.AccessRouteProfile
	if err := c.ShouldBindJSON(&profile); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.AccessRouteProfileCreate(&profile, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, profile)
}

func updateAccessRouteProfile(c *gin.Context) {
	var profile model.AccessRouteProfile
	if err := c.ShouldBindJSON(&profile); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.AccessRouteProfileUpdate(&profile, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, profile)
}

func deleteAccessRouteProfile(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.AccessRouteProfileDelete(id, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func createAccessRouteRule(c *gin.Context) {
	var rule model.AccessRouteRule
	if err := c.ShouldBindJSON(&rule); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.AccessRouteRuleCreate(&rule, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, rule)
}

func updateAccessRouteRule(c *gin.Context) {
	var rule model.AccessRouteRule
	if err := c.ShouldBindJSON(&rule); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.AccessRouteRuleUpdate(&rule, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, rule)
}

func deleteAccessRouteRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.AccessRouteRuleDelete(id, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func createAccessRouteTarget(c *gin.Context) {
	var target model.AccessRouteTarget
	if err := c.ShouldBindJSON(&target); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.AccessRouteTargetCreate(&target, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, target)
}

func updateAccessRouteTarget(c *gin.Context) {
	var batch struct {
		AccessPlanID int                       `json:"access_plan_id"`
		Targets      []model.AccessRouteTarget `json:"targets"`
	}
	if err := c.ShouldBindBodyWith(&batch, binding.JSON); err == nil && batch.AccessPlanID > 0 {
		plan, err := op.AccessPlanUpdateRouteTargets(batch.AccessPlanID, batch.Targets, c.Request.Context())
		if err != nil {
			resp.Error(c, http.StatusInternalServerError, err.Error())
			return
		}
		resp.Success(c, plan)
		return
	}

	var target model.AccessRouteTarget
	if err := c.ShouldBindBodyWith(&target, binding.JSON); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.AccessRouteTargetUpdate(&target, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, target)
}

func deleteAccessRouteTarget(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.AccessRouteTargetDelete(id, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func createAccessBillingProfile(c *gin.Context) {
	var profile model.AccessBillingProfile
	if err := c.ShouldBindJSON(&profile); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.AccessBillingProfileCreate(&profile, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, profile)
}

func updateAccessBillingProfile(c *gin.Context) {
	var profile model.AccessBillingProfile
	if err := c.ShouldBindJSON(&profile); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.AccessBillingProfileUpdate(&profile, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, profile)
}

func deleteAccessBillingProfile(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.AccessBillingProfileDelete(id, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func updateAccessBillingRules(c *gin.Context) {
	var req struct {
		AccessPlanID      int                            `json:"access_plan_id" binding:"required"`
		DefaultMultiplier float64                        `json:"default_multiplier"`
		Rules             []model.AccessBillingModelRule `json:"rules"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	plan, err := op.AccessPlanUpdateBillingRules(req.AccessPlanID, req.DefaultMultiplier, req.Rules, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, plan)
}

func createAccessBillingModelRule(c *gin.Context) {
	var rule model.AccessBillingModelRule
	if err := c.ShouldBindJSON(&rule); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.AccessBillingModelRuleCreate(&rule, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, rule)
}

func updateAccessBillingModelRule(c *gin.Context) {
	var rule model.AccessBillingModelRule
	if err := c.ShouldBindJSON(&rule); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.AccessBillingModelRuleUpdate(&rule, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, rule)
}

func deleteAccessBillingModelRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.AccessBillingModelRuleDelete(id, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func bindAPIKeyAccessPlans(c *gin.Context) {
	apiKeyID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	var req struct {
		AccessPlanIDs       []int `json:"access_plan_ids" binding:"required"`
		DefaultAccessPlanID int   `json:"default_access_plan_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.APIKeyAccessPlanSet(apiKeyID, req.AccessPlanIDs, req.DefaultAccessPlanID, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	apiKey, err := op.APIKeyGet(apiKeyID, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, apiKey)
}
