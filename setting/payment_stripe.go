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

// StripeFeeRate is what Stripe takes from one payment method: Pct of the
// charged amount plus FixedUSD. It is passed on to the payer, so the amount
// charged is grossed up until the net after Stripe's fee equals the face value.
type StripeFeeRate struct {
	Pct      float64 `json:"pct"`
	FixedUSD float64 `json:"fixed_usd"`
}

// StripeFeeRates defaults follow Stripe's US price list: 2.9% + 30c, +1% when
// currency conversion is needed (CNY charges settle in USD), +1.5% for
// international cards (cards of Chinese users are international to a US
// account). Stablecoins are charged in USD, 1.5%.
var StripeFeeRates = map[string]StripeFeeRate{
	"wechat_pay": {Pct: 0.039, FixedUSD: 0.30},
	"alipay":     {Pct: 0.039, FixedUSD: 0.30},
	"card":       {Pct: 0.054, FixedUSD: 0.30},
	"crypto":     {Pct: 0.015},
}
