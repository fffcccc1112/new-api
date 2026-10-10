package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupProvisionTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := openTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&model.User{}, &model.Token{}, &model.SubscriptionPlan{}, &model.UserSubscription{},
		&model.SubscriptionPreConsumeRecord{}, &model.Log{}, &model.AuditLog{},
	))
	return db
}

func withPaymentCompliance(t *testing.T) {
	t.Helper()
	payment := operation_setting.GetPaymentSetting()
	previousConfirmed, previousVersion := payment.ComplianceConfirmed, payment.ComplianceTermsVersion
	payment.ComplianceConfirmed, payment.ComplianceTermsVersion = true, operation_setting.CurrentComplianceTermsVersion
	t.Cleanup(func() {
		payment.ComplianceConfirmed, payment.ComplianceTermsVersion = previousConfirmed, previousVersion
	})
}

func adminContext(t *testing.T, method, path, body string, operatorID, role int, params gin.Params) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	c.Set("id", operatorID)
	c.Set("role", role)
	c.Set("username", "flow-root")
	return c, recorder
}

func TestAdminProvisionFlowBindsDailyPlanAndIssuesKey(t *testing.T) {
	db := setupProvisionTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.CasbinRule{}, &model.AuthzRole{}))
	require.NoError(t, authz.Init(db))
	withPaymentCompliance(t)

	root := &model.User{Username: "flow-root", Password: "password-1", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AffCode: "flow-root"}
	require.NoError(t, db.Create(root).Error)

	// 1. Admin creates a plan whose quota resets every day.
	planCtx, planRecorder := adminContext(t, http.MethodPost, "/api/subscription/admin/plans",
		`{"plan":{"title":"Daily 100","price_amount":1,"duration_unit":"month","duration_value":1,"total_amount":100,"quota_reset_period":"daily","enabled":true}}`,
		root.Id, common.RoleRootUser, nil)
	AdminCreateSubscriptionPlan(planCtx)
	planResponse := decodeAPIResponse(t, planRecorder)
	require.True(t, planResponse.Success, "plan response: %s", planRecorder.Body.String())
	var planData struct {
		ID int `json:"id"`
	}
	require.NoError(t, common.Unmarshal(planResponse.Data, &planData))
	require.Greater(t, planData.ID, 0)

	// 2. Admin registers the user.
	userCtx, userRecorder := adminContext(t, http.MethodPost, "/api/user/",
		`{"username":"flow-student","password":"student-password-1","role":1}`,
		root.Id, common.RoleRootUser, nil)
	CreateUser(userCtx)
	require.True(t, decodeAPIResponse(t, userRecorder).Success, "create user response: %s", userRecorder.Body.String())

	var student model.User
	require.NoError(t, db.Where("username = ?", "flow-student").First(&student).Error)
	assert.Equal(t, common.RoleCommonUser, student.Role)

	// 3. Admin binds the plan and provisions an API key for that user.
	provisionCtx, provisionRecorder := adminContext(t, http.MethodPost, "/api/user/"+strconv.Itoa(student.Id)+"/provision",
		fmt.Sprintf(`{"plan_id":%d,"token_name":"daily-key"}`, planData.ID),
		root.Id, common.RoleRootUser, gin.Params{{Key: "id", Value: strconv.Itoa(student.Id)}})
	AdminProvisionUserToken(provisionCtx)
	provisionResponse := decodeAPIResponse(t, provisionRecorder)
	require.True(t, provisionResponse.Success, "provision response: %s", provisionRecorder.Body.String())
	var provision struct {
		UserID  int    `json:"user_id"`
		TokenID int    `json:"token_id"`
		PlanID  int    `json:"plan_id"`
		Key     string `json:"key"`
	}
	require.NoError(t, common.Unmarshal(provisionResponse.Data, &provision))
	assert.Equal(t, student.Id, provision.UserID)
	assert.Equal(t, planData.ID, provision.PlanID)
	require.True(t, strings.HasPrefix(provision.Key, "sk-"))

	// The returned key is a usable relay key owned by the student.
	var storedToken model.Token
	require.NoError(t, db.Where("id = ?", provision.TokenID).First(&storedToken).Error)
	assert.Equal(t, student.Id, storedToken.UserId)
	assert.Equal(t, strings.TrimPrefix(provision.Key, "sk-"), storedToken.Key)

	// 4. The subscription is active with a daily reset window.
	var sub model.UserSubscription
	require.NoError(t, db.Where("user_id = ? AND plan_id = ?", student.Id, planData.ID).First(&sub).Error)
	assert.Equal(t, "active", sub.Status)
	assert.EqualValues(t, 100, sub.AmountTotal)
	assert.Greater(t, sub.NextResetTime, common.GetTimestamp())

	// 5. Daily allowance is enforced: 30 fits, 80 more does not.
	firstConsume, err := model.PreConsumeUserSubscription("flow-req-1", student.Id, "deepseek-chat", 0, 30)
	require.NoError(t, err)
	assert.EqualValues(t, 30, firstConsume.PreConsumed)
	_, err = model.PreConsumeUserSubscription("flow-req-2", student.Id, "deepseek-chat", 0, 80)
	require.Error(t, err)

	// 6. The next day resets the used amount, so the allowance is available again.
	require.NoError(t, db.Model(&model.UserSubscription{}).Where("id = ?", sub.Id).
		Updates(map[string]any{
			"last_reset_time": common.GetTimestamp() - 2*24*3600,
			"next_reset_time": common.GetTimestamp() - 1,
		}).Error)
	resetCount, err := model.ResetDueSubscriptions(200)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, resetCount, 1)
	require.NoError(t, db.Where("id = ?", sub.Id).First(&sub).Error)
	assert.Zero(t, sub.AmountUsed)
	assert.Greater(t, sub.NextResetTime, common.GetTimestamp())
	_, err = model.PreConsumeUserSubscription("flow-req-3", student.Id, "deepseek-chat", 0, 80)
	require.NoError(t, err)
}

func TestAdminProvisionUserToken(t *testing.T) {
	db := setupProvisionTestDB(t)
	withPaymentCompliance(t)

	admin := &model.User{Username: "provision-admin", Password: "password-1", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AffCode: "prov-admin"}
	student := &model.User{Username: "provision-student", Password: "password-1", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "prov-student"}
	require.NoError(t, db.Create(admin).Error)
	require.NoError(t, db.Create(student).Error)

	plan := &model.SubscriptionPlan{Title: "Daily 100", PriceAmount: 1, DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100, QuotaResetPeriod: model.SubscriptionResetDaily}
	require.NoError(t, db.Create(plan).Error)

	ctx, recorder := adminContext(t, http.MethodPost, "/api/user/"+strconv.Itoa(student.Id)+"/provision",
		fmt.Sprintf(`{"plan_id":%d,"token_name":"provisioned"}`, plan.Id),
		admin.Id, common.RoleRootUser, gin.Params{{Key: "id", Value: strconv.Itoa(student.Id)}})
	AdminProvisionUserToken(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, "response: %s", recorder.Body.String())

	var data struct {
		UserID  int    `json:"user_id"`
		TokenID int    `json:"token_id"`
		PlanID  int    `json:"plan_id"`
		Key     string `json:"key"`
	}
	require.NoError(t, common.Unmarshal(response.Data, &data))
	assert.Equal(t, student.Id, data.UserID)
	assert.Equal(t, plan.Id, data.PlanID)
	require.True(t, strings.HasPrefix(data.Key, "sk-"))

	var stored model.Token
	require.NoError(t, db.Where("id = ?", data.TokenID).First(&stored).Error)
	assert.Equal(t, student.Id, stored.UserId)
	assert.Equal(t, strings.TrimPrefix(data.Key, "sk-"), stored.Key)

	var subscriptionCount int64
	require.NoError(t, db.Model(&model.UserSubscription{}).Where("user_id = ? AND plan_id = ?", student.Id, plan.Id).Count(&subscriptionCount).Error)
	assert.EqualValues(t, 1, subscriptionCount)
}

func TestAdminProvisionUserTokenRejectsEqualOrHigherRole(t *testing.T) {
	db := setupProvisionTestDB(t)

	operator := &model.User{Username: "provision-op", Password: "password-1", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, AffCode: "prov-op"}
	peer := &model.User{Username: "provision-peer", Password: "password-1", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, AffCode: "prov-peer"}
	require.NoError(t, db.Create(operator).Error)
	require.NoError(t, db.Create(peer).Error)

	ctx, recorder := adminContext(t, http.MethodPost, "/api/user/"+strconv.Itoa(peer.Id)+"/provision",
		`{"token_name":"blocked"}`, operator.Id, common.RoleAdminUser, gin.Params{{Key: "id", Value: strconv.Itoa(peer.Id)}})
	AdminProvisionUserToken(ctx)

	response := decodeAPIResponse(t, recorder)
	assert.False(t, response.Success)
	var count int64
	require.NoError(t, db.Model(&model.Token{}).Where("user_id = ?", peer.Id).Count(&count).Error)
	assert.Zero(t, count)
}

func TestAdminProvisionUserCreatesByExternalId(t *testing.T) {
	db := setupProvisionTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.CasbinRule{}, &model.AuthzRole{}))
	require.NoError(t, authz.Init(db))
	withPaymentCompliance(t)

	root := &model.User{Username: "flow-root", Password: "password-1", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AffCode: "flow-root"}
	require.NoError(t, db.Create(root).Error)
	plan := &model.SubscriptionPlan{Id: 1, Title: "Daily", TotalAmount: 100, DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, QuotaResetPeriod: model.SubscriptionResetDaily}
	require.NoError(t, db.Create(plan).Error)

	ctx, recorder := adminContext(t, http.MethodPost, "/api/user/provision",
		`{"external_id":"biz-1001","email":"biz1001@example.com","plan_id":1,"token_name":"first"}`,
		root.Id, common.RoleRootUser, nil)
	AdminProvisionUser(ctx)
	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, "response: %s", recorder.Body.String())

	var first struct {
		UserID   int    `json:"user_id"`
		Username string `json:"username"`
		TokenID  int    `json:"token_id"`
		Key      string `json:"key"`
		Created  bool   `json:"created"`
	}
	require.NoError(t, common.Unmarshal(response.Data, &first))
	assert.True(t, first.Created)
	assert.Equal(t, "biz-1001", first.Username)
	require.True(t, strings.HasPrefix(first.Key, "sk-"))

	var user model.User
	require.NoError(t, db.Where("username = ?", "biz-1001").First(&user).Error)
	assert.Equal(t, common.RoleCommonUser, user.Role)
	assert.Equal(t, first.UserID, user.Id)
	assert.Equal(t, "biz1001@example.com", user.Email)

	var subCount int64
	require.NoError(t, db.Model(&model.UserSubscription{}).Where("user_id = ? AND plan_id = ?", user.Id, plan.Id).Count(&subCount).Error)
	assert.EqualValues(t, 1, subCount)

	// A second call reuses the same account instead of creating a duplicate.
	ctx2, recorder2 := adminContext(t, http.MethodPost, "/api/user/provision",
		`{"external_id":"biz-1001","token_name":"second"}`,
		root.Id, common.RoleRootUser, nil)
	AdminProvisionUser(ctx2)
	response2 := decodeAPIResponse(t, recorder2)
	require.True(t, response2.Success, "response: %s", recorder2.Body.String())
	var second struct {
		UserID  int    `json:"user_id"`
		Created bool   `json:"created"`
		Key     string `json:"key"`
	}
	require.NoError(t, common.Unmarshal(response2.Data, &second))
	assert.False(t, second.Created)
	assert.Equal(t, user.Id, second.UserID)
	assert.NotEqual(t, first.Key, second.Key)

	var userCount int64
	require.NoError(t, db.Model(&model.User{}).Where("username = ?", "biz-1001").Count(&userCount).Error)
	assert.EqualValues(t, 1, userCount)
}

func TestAdminProvisionUserCreatesWithExplicitId(t *testing.T) {
	db := setupProvisionTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.CasbinRule{}, &model.AuthzRole{}))
	require.NoError(t, authz.Init(db))
	withPaymentCompliance(t)

	root := &model.User{Username: "flow-root", Password: "password-1", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AffCode: "flow-root"}
	require.NoError(t, db.Create(root).Error)
	plan := &model.SubscriptionPlan{Id: 1, Title: "Daily", TotalAmount: 100, DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, QuotaResetPeriod: model.SubscriptionResetDaily}
	require.NoError(t, db.Create(plan).Error)

	ctx, recorder := adminContext(t, http.MethodPost, "/api/user/provision",
		`{"user_id":2001,"external_id":"biz-2001","email":"biz2001@example.com","plan_id":1}`,
		root.Id, common.RoleRootUser, nil)
	AdminProvisionUser(ctx)
	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, "response: %s", recorder.Body.String())
	var data struct {
		UserID  int  `json:"user_id"`
		Created bool `json:"created"`
	}
	require.NoError(t, common.Unmarshal(response.Data, &data))
	assert.True(t, data.Created)
	assert.Equal(t, 2001, data.UserID)

	var user model.User
	require.NoError(t, db.Where("id = ?", 2001).First(&user).Error)
	assert.Equal(t, "biz-2001", user.Username)
	assert.Equal(t, "biz2001@example.com", user.Email)

	// A later auto-generated id must not collide with the explicit one.
	auto := &model.User{Username: "auto-after-explicit", Password: "password-1", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "auto-aff"}
	require.NoError(t, auto.Insert(0))
	assert.Greater(t, auto.Id, 2001)
}

func TestAdminProvisionUserTokenUnknownUser(t *testing.T) {
	_ = setupProvisionTestDB(t)

	ctx, recorder := adminContext(t, http.MethodPost, "/api/user/999999/provision",
		`{}`, 1, common.RoleRootUser, gin.Params{{Key: "id", Value: "999999"}})
	AdminProvisionUserToken(ctx)

	response := decodeAPIResponse(t, recorder)
	assert.False(t, response.Success)
}
