package controller

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/go-fuego/fuego"
	"github.com/thanhpk/randstr"
)

func SubscriptionRequestNowPaymentsPay(c fuego.ContextWithBody[dto.SubscriptionNowPaymentsPayRequest]) (*dto.Response[dto.NowPaymentsPayData], error) {
	ginCtx := dto.GinCtx(c)
	if !operation_setting.IsPaymentComplianceConfirmed() {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, i18n.MsgPaymentComplianceRequired))
	}
	if !setting.NowPaymentsSubscriptionEnabled {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "payment.channel_not_supported"))
	}
	req, err := c.Body()
	if err != nil || req.PlanId <= 0 {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "common.invalid_params"))
	}

	plan, err := model.GetSubscriptionPlanById(req.PlanId)
	if err != nil {
		return dto.Fail[dto.NowPaymentsPayData](err.Error())
	}
	if !plan.Enabled {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "subscription.not_enabled"))
	}
	if setting.NowPaymentsApiKey == "" || setting.NowPaymentsIpnSecret == "" {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "payment.webhook_not_configured"))
	}

	userId := dto.UserID(c)
	user, err := model.GetUserById(userId, false)
	if err != nil || user == nil {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "user.not_exists"))
	}
	if user.Email == "" {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "topup.nowpayments_email_required"))
	}

	if plan.MaxPurchasePerUser > 0 {
		count, err := model.CountUserSubscriptionsByPlan(userId, plan.Id)
		if err != nil {
			return dto.Fail[dto.NowPaymentsPayData](err.Error())
		}
		if count >= int64(plan.MaxPurchasePerUser) {
			return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "subscription.purchase_max"))
		}
	}

	npPlanId, err := getOrCreateNowPaymentsPlan(plan)
	if err != nil {
		log.Println(i18n.Translate("topup.nowpayments_create_plan_failed"), err)
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "payment.start_failed"))
	}

	reference := fmt.Sprintf("sub-nowpayments-ref-%d-%d-%s", user.Id, time.Now().UnixMilli(), randstr.String(4))
	referenceId := NowPaymentsSubOrderRefPrefx + common.Sha1([]byte(reference))

	order := &model.SubscriptionOrder{
		UserId:          userId,
		PlanId:          plan.Id,
		Money:           plan.PriceAmount,
		TradeNo:         referenceId,
		PaymentMethod:   PaymentMethodNowPayments,
		PaymentProvider: model.PaymentProviderNowPayments,
		CreateTime:      time.Now().Unix(),
		Status:          common.TopUpStatusPending,
	}
	if err := order.Insert(); err != nil {
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "payment.create_failed"))
	}

	if _, err := createNowPaymentsEmailSubscription(npPlanId, user.Email); err != nil {
		log.Println(i18n.Translate("topup.nowpayments_create_email_sub_failed"), err)
		return dto.Fail[dto.NowPaymentsPayData](common.TranslateMessage(ginCtx, "payment.start_failed"))
	}

	return dto.Ok(dto.NowPaymentsPayData{PayLink: ""})
}

// getOrCreateNowPaymentsPlan returns the NowPayments-side plan id for a given
// local plan, creating it lazily via API if needed and caching on the row.
func getOrCreateNowPaymentsPlan(plan *model.SubscriptionPlan) (string, error) {
	if plan.NowPaymentsPlanId != "" {
		return plan.NowPaymentsPlanId, nil
	}

	intervalDays, err := planDurationDays(plan)
	if err != nil {
		return "", err
	}

	body := dto.NowPaymentsPlanRequest{
		Title:            plan.Title,
		IntervalDay:      fmt.Sprintf("%d", intervalDays),
		Amount:           plan.PriceAmount,
		Currency:         "usd",
		IpnCallbackURL:   service.GetCallbackAddress() + "/api/payment/nowpayments/webhook",
		SuccessURL:       paymentReturnPath("/console/subscription"),
		CancelURL:        paymentReturnPath("/console/subscription"),
		PartiallyPaidURL: paymentReturnPath("/console/subscription"),
	}

	jsonData, err := common.Marshal(body)
	if err != nil {
		return "", err
	}

	apiUrl := nowPaymentsApiBase() + "/subscriptions/plans"
	req, err := http.NewRequest("POST", apiUrl, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", setting.NowPaymentsApiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("nowpayments plan creation failed status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var planResp dto.NowPaymentsPlanResponse
	if err = common.Unmarshal(respBody, &planResp); err != nil {
		return "", err
	}
	if planResp.Result.Id == "" {
		return "", errors.New("nowpayments returned empty plan id")
	}

	plan.NowPaymentsPlanId = planResp.Result.Id
	if err := model.DB.Model(plan).Where("id = ?", plan.Id).Update("nowpayments_plan_id", planResp.Result.Id).Error; err != nil {
		log.Println("nowpayments: failed to cache plan id:", err)
	}
	return planResp.Result.Id, nil
}

func createNowPaymentsEmailSubscription(npPlanId, email string) (string, error) {
	body := dto.NowPaymentsEmailSubRequest{
		SubscriptionPlanId: npPlanId,
		EmailAddresses:     []string{email},
	}
	jsonData, err := common.Marshal(body)
	if err != nil {
		return "", err
	}

	apiUrl := nowPaymentsApiBase() + "/subscriptions"
	req, err := http.NewRequest("POST", apiUrl, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", setting.NowPaymentsApiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("nowpayments email sub failed status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var subResp dto.NowPaymentsEmailSubResponse
	if err = common.Unmarshal(respBody, &subResp); err != nil {
		return "", err
	}
	if len(subResp.Result) == 0 {
		return "", errors.New("nowpayments returned no subscription")
	}
	return subResp.Result[0].Id, nil
}

func planDurationDays(plan *model.SubscriptionPlan) (int, error) {
	switch plan.DurationUnit {
	case model.SubscriptionDurationYear:
		return plan.DurationValue * 365, nil
	case model.SubscriptionDurationMonth:
		return plan.DurationValue * 30, nil
	case model.SubscriptionDurationDay:
		return plan.DurationValue, nil
	case model.SubscriptionDurationHour:
		days := plan.DurationValue / 24
		if days < 1 {
			days = 1
		}
		return days, nil
	case model.SubscriptionDurationCustom:
		days := int(plan.CustomSeconds / 86400)
		if days < 1 {
			days = 1
		}
		return days, nil
	default:
		return 0, fmt.Errorf("unsupported duration unit for nowpayments: %s", plan.DurationUnit)
	}
}
