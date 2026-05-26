package controller

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
	"github.com/go-fuego/fuego"
	"github.com/thanhpk/randstr"
)

const (
	PaymentMethodNowPayments    = "nowpayments"
	NowPaymentsSignatureHeader  = "x-nowpayments-sig"
	NowPaymentsApiBaseProd      = "https://api.nowpayments.io/v1"
	NowPaymentsApiBaseSandbox   = "https://api-sandbox.nowpayments.io/v1"
	NowPaymentsTopUpRefPrefix   = "ref_"
	NowPaymentsSubOrderRefPrefx = "subref_"
)

func nowPaymentsApiBase() string {
	if setting.NowPaymentsSandbox {
		return NowPaymentsApiBaseSandbox
	}
	return NowPaymentsApiBaseProd
}

func RequestNowPaymentsAmount(c fuego.ContextWithBody[dto.NowPaymentsPayRequest]) (*dto.Response[string], error) {
	ginCtx := dto.GinCtx(c)
	req, err := c.Body()
	if err != nil {
		return dto.Fail[string](common.TranslateMessage(ginCtx, "common.invalid_params"))
	}
	if req.Amount < getNowPaymentsMinTopup() {
		return dto.Fail[string](common.TranslateMessage(ginCtx, "topup.min_amount", map[string]any{"Amount": getNowPaymentsMinTopup()}))
	}
	id := dto.UserID(c)
	group, err := model.GetUserGroup(id, true)
	if err != nil {
		return dto.Fail[string](common.TranslateMessage(ginCtx, "topup.get_group_failed"))
	}
	payMoney := getNowPaymentsPayMoney(float64(req.Amount), group)
	if payMoney <= 0.01 {
		return dto.Fail[string](common.TranslateMessage(ginCtx, "topup.amount_too_low"))
	}
	return dto.Ok(strconv.FormatFloat(payMoney, 'f', 2, 64))
}

func RequestNowPaymentsPay(c fuego.ContextWithBody[dto.NowPaymentsPayRequest]) (*dto.Response[dto.NowPaymentsPayData], error) {
	ginCtx := dto.GinCtx(c)
	req, err := c.Body()
	if err != nil {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "common.invalid_params"))
	}
	if req.PaymentMethod != PaymentMethodNowPayments {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "payment.channel_not_supported"))
	}
	if req.Amount < getNowPaymentsMinTopup() {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "topup.min_amount", map[string]any{"Amount": getNowPaymentsMinTopup()}))
	}
	if req.Amount > 10000 {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "topup.max_amount"))
	}

	if req.SuccessURL != "" && common.ValidateRedirectURL(req.SuccessURL) != nil {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "topup.success_redirect_untrusted"))
	}
	if req.CancelURL != "" && common.ValidateRedirectURL(req.CancelURL) != nil {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "topup.cancel_redirect_untrusted"))
	}

	id := dto.UserID(c)
	user, err := model.GetUserById(id, false)
	if err != nil || user == nil {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "topup.get_user_failed"))
	}
	chargedMoney := GetChargedAmount(float64(req.Amount), *user)

	reference := fmt.Sprintf("nowpayments-ref-%d-%d-%s", user.Id, time.Now().UnixMilli(), randstr.String(4))
	referenceId := NowPaymentsTopUpRefPrefix + common.Sha1([]byte(reference))

	payLink, err := genNowPaymentsInvoice(referenceId, chargedMoney, req.SuccessURL, req.CancelURL, fmt.Sprintf("new-api topup %d units", req.Amount))
	if err != nil {
		log.Println(i18n.Translate("topup.nowpayments_get_pay_link_failed"), err)
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "payment.start_failed"))
	}

	topUp := &model.TopUp{
		UserId:          id,
		Amount:          req.Amount,
		Money:           chargedMoney,
		TradeNo:         referenceId,
		PaymentMethod:   PaymentMethodNowPayments,
		PaymentProvider: model.PaymentProviderNowPayments,
		CreateTime:      time.Now().Unix(),
		Status:          common.TopUpStatusPending,
	}
	if err = topUp.Insert(); err != nil {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "payment.create_failed"))
	}
	return dto.Ok(dto.NowPaymentsPayData{PayLink: payLink})
}

func genNowPaymentsInvoice(referenceId string, payMoney float64, successURL, cancelURL, description string) (string, error) {
	if setting.NowPaymentsApiKey == "" {
		return "", errors.New(i18n.Translate("topup.nowpayments_key_not_configured"))
	}

	if successURL == "" {
		successURL = paymentReturnPath("/console/log")
	}
	if cancelURL == "" {
		cancelURL = paymentReturnPath("/console/topup")
	}

	body := dto.NowPaymentsInvoiceRequest{
		PriceAmount:      payMoney,
		PriceCurrency:    "usd",
		OrderId:          referenceId,
		OrderDescription: description,
		IpnCallbackURL:   service.GetCallbackAddress() + "/api/payment/nowpayments/webhook",
		SuccessURL:       successURL,
		CancelURL:        cancelURL,
		IsFixedRate:      setting.NowPaymentsIsFixedRate,
		IsFeePaidByUser:  setting.NowPaymentsFeePaidByUser,
	}
	jsonData, err := common.Marshal(body)
	if err != nil {
		return "", fmt.Errorf(i18n.Translate("topup.nowpayments_marshal_failed", map[string]any{"Error": err.Error()}))
	}

	apiUrl := nowPaymentsApiBase() + "/invoice"
	req, err := http.NewRequest("POST", apiUrl, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf(i18n.Translate("topup.nowpayments_create_req_failed", map[string]any{"Error": err.Error()}))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", setting.NowPaymentsApiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf(i18n.Translate("topup.nowpayments_send_req_failed", map[string]any{"Error": err.Error()}))
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf(i18n.Translate("topup.nowpayments_read_resp_failed", map[string]any{"Error": err.Error()}))
	}

	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("nowpayments invoice creation failed status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var invoiceResp dto.NowPaymentsInvoiceResponse
	if err = common.Unmarshal(respBody, &invoiceResp); err != nil {
		return "", fmt.Errorf(i18n.Translate("topup.nowpayments_parse_resp_failed", map[string]any{"Error": err.Error()}))
	}
	if invoiceResp.InvoiceURL == "" {
		return "", errors.New("nowpayments returned empty invoice_url")
	}
	return invoiceResp.InvoiceURL, nil
}

// canonicalJSONForNowPaymentsHMAC re-marshals payload with sorted keys for
// HMAC-SHA512 signature parity with NowPayments. Direct encoding/json use
// (Rule 1 exception) is required because byte-exact key ordering matters.
func canonicalJSONForNowPaymentsHMAC(payload []byte) ([]byte, error) {
	dec := stdjson.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	return stdjson.Marshal(obj)
}

func verifyNowPaymentsSignature(payload []byte, sig, secret string) bool {
	if secret == "" || sig == "" {
		return false
	}
	sorted, err := canonicalJSONForNowPaymentsHMAC(payload)
	if err != nil {
		return false
	}
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write(sorted)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected))
}

func NowPaymentsWebhook(c *gin.Context) {
	ctx := c.Request.Context()
	callerIp := c.ClientIP()

	if setting.NowPaymentsIpnSecret == "" {
		logger.LogWarn(ctx, fmt.Sprintf("NowPayments webhook 未配置 IPN secret，拒绝处理 client_ip=%s", callerIp))
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("NowPayments webhook 读取 payload 失败 client_ip=%s error=%q", callerIp, err.Error()))
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	signature := c.GetHeader(NowPaymentsSignatureHeader)
	if signature == "" {
		logger.LogWarn(ctx, fmt.Sprintf("NowPayments webhook 缺少签名 client_ip=%s", callerIp))
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	if !verifyNowPaymentsSignature(bodyBytes, signature, setting.NowPaymentsIpnSecret) {
		logger.LogWarn(ctx, fmt.Sprintf("NowPayments webhook 签名校验失败 client_ip=%s", callerIp))
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	var event dto.NowPaymentsWebhookEvent
	if err = common.Unmarshal(bodyBytes, &event); err != nil {
		logger.LogError(ctx, fmt.Sprintf("NowPayments webhook 解析 payload 失败 client_ip=%s error=%q", callerIp, err.Error()))
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	logger.LogInfo(ctx, fmt.Sprintf("NowPayments webhook 收到事件 order_id=%s status=%s pay_currency=%s actually_paid=%v client_ip=%s",
		event.OrderId, event.PaymentStatus, event.PayCurrency, event.ActuallyPaid, callerIp))

	if event.OrderId == "" {
		logger.LogWarn(ctx, fmt.Sprintf("NowPayments webhook 缺少 order_id client_ip=%s", callerIp))
		c.Status(http.StatusOK)
		return
	}

	handleNowPaymentsEvent(c, &event, callerIp)
}

func handleNowPaymentsEvent(c *gin.Context, event *dto.NowPaymentsWebhookEvent, callerIp string) {
	ctx := c.Request.Context()
	orderId := event.OrderId
	status := event.PaymentStatus

	switch status {
	case "finished":
		LockOrder(orderId)
		defer UnlockOrder(orderId)

		subPayload := common.GetJsonString(event)
		if err := model.CompleteSubscriptionOrder(orderId, subPayload, model.PaymentProviderNowPayments, ""); err == nil {
			logger.LogInfo(ctx, fmt.Sprintf("NowPayments 订阅订单处理成功 trade_no=%s client_ip=%s", orderId, callerIp))
			if topUp := model.GetTopUpByTradeNo(orderId); topUp == nil {
				c.Status(http.StatusOK)
				return
			}
		} else if !errors.Is(err, model.ErrSubscriptionOrderNotFound) {
			logger.LogError(ctx, fmt.Sprintf("NowPayments 订阅订单处理失败 trade_no=%s error=%q", orderId, err.Error()))
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}

		if err := model.RechargeNowPayments(orderId, event.PayCurrency, event.ActuallyPaid); err != nil {
			logger.LogError(ctx, fmt.Sprintf("NowPayments 充值处理失败 trade_no=%s client_ip=%s error=%q", orderId, callerIp, err.Error()))
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}

		if topUp := model.GetTopUpByTradeNo(orderId); topUp != nil {
			go service.SendTopupConfirmationEmail(topUp.UserId, topUp.Money, topUp.Amount, event.PayCurrency, topUp.TradeNo)
		}
		c.Status(http.StatusOK)

	case "failed", "expired", "refunded":
		LockOrder(orderId)
		defer UnlockOrder(orderId)
		err := model.UpdatePendingTopUpStatus(orderId, model.PaymentProviderNowPayments, common.TopUpStatusFailed)
		if err != nil && !errors.Is(err, model.ErrTopUpNotFound) && !errors.Is(err, model.ErrTopUpStatusInvalid) {
			logger.LogError(ctx, fmt.Sprintf("NowPayments 失败状态标记失败 trade_no=%s status=%s error=%q", orderId, status, err.Error()))
		} else {
			logger.LogInfo(ctx, fmt.Sprintf("NowPayments 充值订单已标记 status=%s trade_no=%s", status, orderId))
		}
		c.Status(http.StatusOK)

	case "waiting", "confirming", "confirmed", "sending", "partially_paid":
		logger.LogInfo(ctx, fmt.Sprintf("NowPayments 等待确认 trade_no=%s status=%s", orderId, status))
		c.Status(http.StatusOK)

	default:
		logger.LogWarn(ctx, fmt.Sprintf("NowPayments 未知状态忽略 trade_no=%s status=%s", orderId, status))
		c.Status(http.StatusOK)
	}
}

func getNowPaymentsPayMoney(amount float64, group string) float64 {
	originalAmount := amount
	if operation_setting.GetQuotaDisplayType() == operation_setting.QuotaDisplayTypeTokens {
		amount = amount / common.QuotaPerUnit
	}
	topupGroupRatio := common.GetTopupGroupRatio(group)
	if topupGroupRatio == 0 {
		topupGroupRatio = 1
	}
	discount := 1.0
	if ds, ok := operation_setting.GetPaymentSetting().AmountDiscount[int(originalAmount)]; ok {
		if ds > 0 {
			discount = ds
		}
	}
	return amount * setting.NowPaymentsUnitPrice * topupGroupRatio * discount
}

func getNowPaymentsMinTopup() int64 {
	minTopup := setting.NowPaymentsMinTopUp
	if operation_setting.GetQuotaDisplayType() == operation_setting.QuotaDisplayTypeTokens {
		minTopup = minTopup * int(common.QuotaPerUnit)
	}
	return int64(minTopup)
}
