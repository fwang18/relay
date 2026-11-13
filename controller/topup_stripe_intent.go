package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/paymentintent"
	"github.com/thanhpk/randstr"
)

// In-page payment: the wallet page picks a payment method and confirms a
// PaymentIntent with Stripe.js, instead of redirecting to a hosted Checkout
// page. Credit is still granted only by the signed webhook.

// stripeIntentMethods are the payment methods the in-page flow can create.
var stripeIntentMethods = []string{"wechat_pay", "alipay", "card", "crypto"}

type StripeIntentRequest struct {
	Amount int64  `json:"amount"`
	Method string `json:"method"`
}

// enabledStripeIntentMethods returns the methods configured in StripeInlineMethods,
// kept in the display order of stripeIntentMethods.
func enabledStripeIntentMethods() []string {
	configured := strings.Split(setting.StripeInlineMethods, ",")
	methods := make([]string, 0, len(stripeIntentMethods))
	for _, m := range stripeIntentMethods {
		if slices.ContainsFunc(configured, func(c string) bool { return strings.TrimSpace(c) == m }) {
			methods = append(methods, m)
		}
	}
	return methods
}

func isStripeIntentEnabled() bool {
	if !isPaymentComplianceConfirmed() {
		return false
	}
	hasKey := strings.HasPrefix(setting.StripeApiSecret, "sk_") || strings.HasPrefix(setting.StripeApiSecret, "rk_")
	return hasKey &&
		strings.TrimSpace(setting.StripeWebhookSecret) != "" &&
		strings.TrimSpace(setting.StripePublishableKey) != ""
}

// GetStripeIntentConfig tells the wallet page what it may offer.
func GetStripeIntentConfig(c *gin.Context) {
	if !isStripeIntentEnabled() {
		paymentError(c, common.NewMessage("Stripe is not configured or key is invalid"))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "success",
		"data": gin.H{
			"publishable_key":    setting.StripePublishableKey,
			"methods":            enabledStripeIntentMethods(),
			"crypto_usd_per_cny": setting.StripeCryptoUsdPerCny,
			"min_topup":          getStripeMinTopup(),
			"max_topup":          int64(10000),
		},
	})
}

// stripeIntentCharge returns the currency, the amount to charge and the part of
// it that is Stripe's fee, all in the smallest currency unit. Card, WeChat Pay
// and Alipay charge CNY (same as the old Checkout price); crypto only supports
// USD, so the CNY amount is converted first. Stripe's fee is passed on to the
// payer: the charge is grossed up so that after Stripe's percentage and fixed
// fee are taken, the net equals the discounted face value (always rounded up
// to the cent). The returned fee is the part above the discounted face value.
func stripeIntentCharge(amount int64, method string) (string, int64, int64, error) {
	face := decimal.NewFromInt(amount).Mul(decimal.NewFromFloat(setting.StripeUnitPrice))
	// Small price cut: pay face value minus StripeTopUpDiscountCny (credit is unchanged).
	if discount := decimal.NewFromFloat(setting.StripeTopUpDiscountCny); discount.IsPositive() && face.Sub(discount).IsPositive() {
		face = face.Sub(discount)
	}
	usdPerCny := decimal.NewFromFloat(setting.StripeCryptoUsdPerCny)
	currency := "cny"
	if method == "crypto" {
		if !usdPerCny.IsPositive() {
			return "", 0, 0, errors.New("crypto exchange rate is not configured")
		}
		face = face.Mul(usdPerCny)
		currency = "usd"
	}
	fee := setting.StripeFeeRates[method]
	if fee.Pct < 0 || fee.Pct >= 1 || fee.FixedUSD < 0 {
		return "", 0, 0, errors.New("invalid Stripe fee rate")
	}
	fixed := decimal.NewFromFloat(fee.FixedUSD)
	if currency == "cny" && fixed.IsPositive() {
		if !usdPerCny.IsPositive() {
			return "", 0, 0, errors.New("exchange rate is not configured")
		}
		fixed = fixed.Div(usdPerCny)
	}
	gross := face.Add(fixed).Div(decimal.NewFromInt(1).Sub(decimal.NewFromFloat(fee.Pct)))
	hundred := decimal.NewFromInt(100)
	minor := gross.Mul(hundred).RoundCeil(0).IntPart()
	if minor < 50 {
		return "", 0, 0, errors.New("payment amount is too low")
	}
	return currency, minor, minor - face.Mul(hundred).Round(0).IntPart(), nil
}

// GetStripeIntentQuote tells the wallet page what each method would charge for
// an amount, fee included, so the price is shown before the user pays.
func GetStripeIntentQuote(c *gin.Context) {
	if !isStripeIntentEnabled() {
		paymentError(c, common.NewMessage("Stripe is not configured or key is invalid"))
		return
	}
	amount, err := strconv.ParseInt(c.Query("amount"), 10, 64)
	if err != nil || amount < getStripeMinTopup() || amount > 10000 {
		paymentError(c, common.NewMessage("Invalid parameters"))
		return
	}
	quotes := gin.H{}
	for _, method := range enabledStripeIntentMethods() {
		currency, charge, fee, err := stripeIntentCharge(amount, method)
		if err != nil {
			continue
		}
		quotes[method] = gin.H{"currency": currency, "charge_amount": charge, "fee_amount": fee}
	}
	c.JSON(http.StatusOK, gin.H{"message": "success", "data": quotes})
}

// RequestStripeIntent creates a pending top-up order and a PaymentIntent for it.
func RequestStripeIntent(c *gin.Context) {
	var req StripeIntentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		paymentError(c, common.NewMessage("Invalid parameters"))
		return
	}
	if !isStripeIntentEnabled() {
		paymentError(c, common.NewMessage("Stripe is not configured or key is invalid"))
		return
	}
	if !slices.Contains(enabledStripeIntentMethods(), req.Method) {
		paymentError(c, common.NewMessage("Unsupported payment channel"))
		return
	}
	if req.Amount < getStripeMinTopup() {
		paymentError(c, common.NewMessage("Top-up amount cannot be less than {{min}}", map[string]any{"min": getStripeMinTopup()}))
		return
	}
	if req.Amount > 10000 {
		paymentError(c, common.NewMessage("Top-up amount cannot exceed {{max}}", map[string]any{"max": 10000}))
		return
	}

	id := c.GetInt("id")
	user, err := model.GetUserById(id, false)
	if err != nil || user == nil {
		paymentError(c, common.NewMessage("User does not exist"))
		return
	}
	chargedMoney := GetChargedAmount(float64(req.Amount), *user)
	if rejectInvalidCreditedQuota(c, id,
		decimal.NewFromFloat(chargedMoney).Mul(decimal.NewFromFloat(common.QuotaPerUnit)),
	) {
		return
	}
	currency, minor, feeMinor, err := stripeIntentCharge(req.Amount, req.Method)
	if err != nil {
		paymentError(c, common.NewMessage("Failed to start payment"))
		return
	}

	reference := fmt.Sprintf("new-api-ref-%d-%d-%s", user.Id, time.Now().UnixMilli(), randstr.String(4))
	tradeNo := "ref_" + common.Sha1([]byte(reference))
	topUp := &model.TopUp{
		UserId:          id,
		Amount:          req.Amount,
		Money:           chargedMoney,
		TradeNo:         tradeNo,
		PaymentMethod:   req.Method,
		PaymentProvider: model.PaymentProviderStripe,
		CreateTime:      time.Now().Unix(),
		Status:          common.TopUpStatusPending,
	}
	if err := topUp.Insert(); err != nil {
		logger.LogError(c.Request.Context(), common.LogText("Stripe failed to create top-up order user_id=%d trade_no=%s amount=%d error=%q", id, tradeNo, req.Amount, err.Error()))
		paymentError(c, common.NewMessage("Failed to create order"))
		return
	}

	stripe.Key = setting.StripeApiSecret
	params := &stripe.PaymentIntentParams{
		Amount:             stripe.Int64(minor),
		Currency:           stripe.String(currency),
		PaymentMethodTypes: stripe.StringSlice([]string{req.Method}),
	}
	params.AddMetadata("trade_no", tradeNo)
	params.AddMetadata("user_id", strconv.Itoa(id))
	params.AddMetadata("expected_amount", strconv.FormatInt(minor, 10))
	params.AddMetadata("expected_currency", currency)
	if req.Method == "wechat_pay" {
		params.PaymentMethodOptions = &stripe.PaymentIntentPaymentMethodOptionsParams{
			WeChatPay: &stripe.PaymentIntentPaymentMethodOptionsWeChatPayParams{
				Client: stripe.String("web"),
			},
		}
	}
	params.SetIdempotencyKey(tradeNo)
	pi, err := paymentintent.New(params)
	if err != nil {
		logger.LogError(c.Request.Context(), common.LogText("Stripe failed to create PaymentIntent user_id=%d trade_no=%s method=%s error=%q", id, tradeNo, req.Method, err.Error()))
		_ = model.UpdatePendingTopUpStatus(tradeNo, model.PaymentProviderStripe, common.TopUpStatusFailed)
		paymentError(c, common.NewMessage("Failed to start payment"))
		return
	}

	logger.LogInfo(c.Request.Context(), common.LogText("Stripe in-page top-up order created user_id=%d trade_no=%s method=%s amount=%d money=%.2f charge=%d%s fee=%d", id, tradeNo, req.Method, req.Amount, chargedMoney, minor, currency, feeMinor))
	c.JSON(http.StatusOK, gin.H{
		"message": "success",
		"data": gin.H{
			"trade_no":      tradeNo,
			"client_secret": pi.ClientSecret,
			"currency":      currency,
			"charge_amount": minor,
			"fee_amount":    feeMinor,
			"money":         chargedMoney,
		},
	})
}

// GetStripeIntentStatus lets the wallet page poll an order until the webhook
// has credited it. Only the order's owner can read it.
func GetStripeIntentStatus(c *gin.Context) {
	topUp := model.GetTopUpByTradeNo(c.Query("trade_no"))
	if topUp == nil || topUp.UserId != c.GetInt("id") || topUp.PaymentProvider != model.PaymentProviderStripe {
		paymentError(c, common.NewMessage("Top-up order does not exist"))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "success",
		"data": gin.H{
			"trade_no": topUp.TradeNo,
			"status":   topUp.Status,
			"money":    topUp.Money,
			"method":   topUp.PaymentMethod,
		},
	})
}

// paymentIntentSucceeded credits the order named by the PaymentIntent metadata.
func paymentIntentSucceeded(ctx context.Context, event stripe.Event, callerIp string) {
	var pi stripe.PaymentIntent
	if err := common.Unmarshal(event.Data.Raw, &pi); err != nil {
		logger.LogError(ctx, common.LogText("Stripe payment_intent.succeeded payload invalid client_ip=%s error=%q", callerIp, err.Error()))
		return
	}
	tradeNo := pi.Metadata["trade_no"]
	if tradeNo == "" {
		// Not one of ours (for example a Checkout Session's own PaymentIntent).
		logger.LogInfo(ctx, common.LogText("Stripe payment_intent.succeeded without trade_no, ignored pi=%s client_ip=%s", pi.ID, callerIp))
		return
	}

	LockOrder(tradeNo)
	defer UnlockOrder(tradeNo)

	topUp := model.GetTopUpByTradeNo(tradeNo)
	if topUp == nil || topUp.PaymentProvider != model.PaymentProviderStripe {
		logger.LogWarn(ctx, common.LogText("Stripe payment_intent.succeeded but local order not found trade_no=%s pi=%s client_ip=%s", tradeNo, pi.ID, callerIp))
		return
	}
	if topUp.Status == common.TopUpStatusSuccess {
		logger.LogInfo(ctx, common.LogText("Stripe payment_intent.succeeded for an order already credited, ignored trade_no=%s pi=%s", tradeNo, pi.ID))
		return
	}
	expectedAmount := pi.Metadata["expected_amount"]
	expectedCurrency := pi.Metadata["expected_currency"]
	if expectedAmount != strconv.FormatInt(pi.AmountReceived, 10) || expectedCurrency != string(pi.Currency) || pi.Status != stripe.PaymentIntentStatusSucceeded {
		logger.LogError(ctx, common.LogText("Stripe payment_intent.succeeded does not match the order, not credited trade_no=%s pi=%s received=%d currency=%s expected=%s%s", tradeNo, pi.ID, pi.AmountReceived, pi.Currency, expectedAmount, expectedCurrency))
		return
	}

	if err := model.Recharge(tradeNo, "", callerIp); err != nil {
		logger.LogError(ctx, common.LogText("Stripe in-page top-up processing failed trade_no=%s pi=%s client_ip=%s error=%q", tradeNo, pi.ID, callerIp, err.Error()))
		return
	}
	logger.LogInfo(ctx, common.LogText("Stripe in-page top-up succeeded trade_no=%s pi=%s amount=%d currency=%s client_ip=%s", tradeNo, pi.ID, pi.AmountReceived, pi.Currency, callerIp))
}

// paymentIntentCanceled marks a never-paid in-page order as expired.
func paymentIntentCanceled(ctx context.Context, event stripe.Event) {
	var pi stripe.PaymentIntent
	if err := common.Unmarshal(event.Data.Raw, &pi); err != nil {
		return
	}
	tradeNo := pi.Metadata["trade_no"]
	if tradeNo == "" {
		return
	}
	LockOrder(tradeNo)
	defer UnlockOrder(tradeNo)
	err := model.UpdatePendingTopUpStatus(tradeNo, model.PaymentProviderStripe, common.TopUpStatusExpired)
	if err != nil && !errors.Is(err, model.ErrTopUpNotFound) && !errors.Is(err, model.ErrTopUpStatusInvalid) {
		logger.LogError(ctx, common.LogText("Stripe in-page top-up cancel processing failed trade_no=%s error=%q", tradeNo, err.Error()))
		return
	}
	logger.LogInfo(ctx, common.LogText("Stripe in-page top-up order canceled trade_no=%s", tradeNo))
}
