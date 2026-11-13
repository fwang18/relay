package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v81"
	"gorm.io/gorm"
)

func setupStripeIntentTest(t *testing.T) *gorm.DB {
	t.Helper()
	oldDB, oldLogDB, oldQuotaPerUnit := model.DB, model.LOG_DB, common.QuotaPerUnit
	oldUnitPrice, oldRate := setting.StripeUnitPrice, setting.StripeCryptoUsdPerCny
	oldRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	common.QuotaPerUnit = 500000
	setting.StripeUnitPrice = 1
	setting.StripeCryptoUsdPerCny = 0.138889

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.TopUp{}, &model.Log{}))
	model.DB, model.LOG_DB = db, db
	t.Cleanup(func() {
		model.DB, model.LOG_DB, common.QuotaPerUnit = oldDB, oldLogDB, oldQuotaPerUnit
		common.RedisEnabled = oldRedisEnabled
		setting.StripeUnitPrice, setting.StripeCryptoUsdPerCny = oldUnitPrice, oldRate
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})

	require.NoError(t, db.Create(&model.User{Id: 7, Username: "payer", AffCode: "aff7", Quota: 0, Status: common.UserStatusEnabled, StripeCustomer: "cus_keep"}).Error)
	require.NoError(t, db.Create(&model.User{Id: 8, Username: "other", AffCode: "aff8", Status: common.UserStatusEnabled}).Error)
	require.NoError(t, db.Create(&model.TopUp{
		UserId: 7, Amount: 10, Money: 10, TradeNo: "ref_order1", PaymentMethod: "wechat_pay",
		PaymentProvider: model.PaymentProviderStripe, Status: common.TopUpStatusPending,
	}).Error)
	return db
}

func intentSucceededEvent(tradeNo string, amount int64, currency string) stripe.Event {
	raw := `{"id":"pi_test","status":"succeeded","amount":` + strconv.FormatInt(amount, 10) + `,"amount_received":` + strconv.FormatInt(amount, 10) +
		`,"currency":"` + currency + `","metadata":{"trade_no":"` + tradeNo + `","expected_amount":"1000","expected_currency":"cny"}}`
	return stripe.Event{Type: stripe.EventTypePaymentIntentSucceeded, Data: &stripe.EventData{Raw: []byte(raw)}}
}

func TestPaymentIntentSucceededCreditsOnceAndChecksAmount(t *testing.T) {
	db := setupStripeIntentTest(t)
	ctx := context.Background()
	quotaOf := func() int {
		var u model.User
		require.NoError(t, db.First(&u, 7).Error)
		return u.Quota
	}

	// Amount that does not match the order is not credited.
	paymentIntentSucceeded(ctx, intentSucceededEvent("ref_order1", 999, "cny"), "1.1.1.1")
	assert.Equal(t, 0, quotaOf())
	// Wrong currency is not credited.
	paymentIntentSucceeded(ctx, intentSucceededEvent("ref_order1", 1000, "usd"), "1.1.1.1")
	assert.Equal(t, 0, quotaOf())
	// An unknown order is ignored.
	paymentIntentSucceeded(ctx, intentSucceededEvent("ref_missing", 1000, "cny"), "1.1.1.1")
	assert.Equal(t, 0, quotaOf())

	// Correct event credits CNY 10 as $10 of quota.
	paymentIntentSucceeded(ctx, intentSucceededEvent("ref_order1", 1000, "cny"), "1.1.1.1")
	assert.Equal(t, 5_000_000, quotaOf())

	// Redelivery of the same event does not credit again.
	paymentIntentSucceeded(ctx, intentSucceededEvent("ref_order1", 1000, "cny"), "1.1.1.1")
	assert.Equal(t, 5_000_000, quotaOf())

	var topUp model.TopUp
	require.NoError(t, db.Where("trade_no = ?", "ref_order1").First(&topUp).Error)
	assert.Equal(t, common.TopUpStatusSuccess, topUp.Status)

	// An existing Stripe customer on the user is kept.
	var user model.User
	require.NoError(t, db.First(&user, 7).Error)
	assert.Equal(t, "cus_keep", user.StripeCustomer)
}

func TestPaymentIntentCanceledOnlyExpiresPendingOrders(t *testing.T) {
	db := setupStripeIntentTest(t)
	raw := `{"id":"pi_test","metadata":{"trade_no":"ref_order1"}}`
	canceled := stripe.Event{Type: stripe.EventTypePaymentIntentCanceled, Data: &stripe.EventData{Raw: []byte(raw)}}

	paymentIntentCanceled(context.Background(), canceled)
	var topUp model.TopUp
	require.NoError(t, db.Where("trade_no = ?", "ref_order1").First(&topUp).Error)
	assert.Equal(t, common.TopUpStatusExpired, topUp.Status)

	// A paid order cannot be turned back by a late cancel event.
	require.NoError(t, db.Model(&topUp).Update("status", common.TopUpStatusSuccess).Error)
	paymentIntentCanceled(context.Background(), canceled)
	require.NoError(t, db.Where("trade_no = ?", "ref_order1").First(&topUp).Error)
	assert.Equal(t, common.TopUpStatusSuccess, topUp.Status)
}

func TestStripeIntentStatusOnlyForOrderOwner(t *testing.T) {
	setupStripeIntentTest(t)
	gin.SetMode(gin.TestMode)
	call := func(userId int, tradeNo string) string {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("id", userId)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/user/stripe/intent/status?trade_no="+tradeNo, nil)
		GetStripeIntentStatus(ctx)
		require.Equal(t, http.StatusOK, recorder.Code)
		return recorder.Body.String()
	}
	assert.JSONEq(t, `{"message":"success","data":{"trade_no":"ref_order1","status":"pending","money":10,"method":"wechat_pay"}}`, call(7, "ref_order1"))
	assert.Contains(t, call(8, "ref_order1"), `"message":"error"`)
	assert.Contains(t, call(7, "ref_nope"), `"message":"error"`)
}

func TestStripeIntentChargePassesStripeFeeToPayer(t *testing.T) {
	setupStripeIntentTest(t)
	oldRates := setting.StripeFeeRates
	t.Cleanup(func() { setting.StripeFeeRates = oldRates })

	tests := []struct {
		name     string
		amount   int64
		method   string
		rates    map[string]setting.StripeFeeRate
		currency string
		minor    int64
		fee      int64
		wantErr  bool
	}{
		// (10 + 0.30/0.138889) / (1-0.039) = 12.653..., rounded up to the fen
		{"wechat grossed up in CNY", 10, "wechat_pay", oldRates, "cny", 1266, 266, false},
		{"card includes international and conversion fee", 100, "card", oldRates, "cny", 10800, 800, false},
		{"crypto converts to USD then adds its fee", 100, "crypto", oldRates, "usd", 1411, 22, false},
		{"no fee configured charges face value", 10, "wechat_pay", map[string]setting.StripeFeeRate{}, "cny", 1000, 0, false},
		{"fee rate of 100% is rejected", 10, "card", map[string]setting.StripeFeeRate{"card": {Pct: 1}}, "", 0, 0, true},
		{"too small to charge", 0, "card", map[string]setting.StripeFeeRate{}, "", 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setting.StripeFeeRates = tt.rates
			currency, minor, fee, err := stripeIntentCharge(tt.amount, tt.method)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.currency, currency)
			assert.Equal(t, tt.minor, minor)
			assert.Equal(t, tt.fee, fee)
		})
	}
}
