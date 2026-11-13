package setting

var StripeApiSecret = ""
var StripeWebhookSecret = ""
var StripePriceId = ""
var StripeUnitPrice = 8.0
var StripeMinTopUp = 1
var StripePromotionCodesEnabled = false

// In-page payment (PaymentIntent) settings.
var StripePublishableKey = ""
var StripeInlineMethods = "wechat_pay,card,crypto"

// StripeCryptoUsdPerCny converts a CNY amount to USD for crypto payments,
// which Stripe only accepts in USD. Default is about CNY 7.2 = USD 1.
var StripeCryptoUsdPerCny = 0.138889
