package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrKind categorizes upstream Qoder and transport errors for pool scheduling & hints.
type ErrKind string

const (
	ErrNone           ErrKind = ""
	ErrSoftRate       ErrKind = "soft_rate"
	ErrHardCredit     ErrKind = "hard_credit"
	ErrSessionDead    ErrKind = "session_dead"
	ErrModelBlocked   ErrKind = "model_blocked"
	ErrAccountFault   ErrKind = "account_fault"
	ErrWafBlock       ErrKind = "waf_block"
	ErrContentBlocked ErrKind = "content_blocked"
	ErrPromptTooLong  ErrKind = "prompt_too_long"
	ErrImageInvalid   ErrKind = "image_invalid"
	ErrBadParams      ErrKind = "bad_params"
	ErrClient         ErrKind = "client"
	ErrNotFound       ErrKind = "not_found"
	ErrServer         ErrKind = "server"
	ErrQueueWait      ErrKind = "upstream_queued"
)

// ModelBlockReason is stored in pool cooldown metadata for model-level disables.
const ModelBlockReason = "model not supported or quota exhausted on account"

// Error represents a structured classified upstream/transport error.
type Error struct {
	Kind       ErrKind       `json:"kind"`
	Status     int           `json:"status"`
	Msg        string        `json:"message"`
	Code       string        `json:"code,omitempty"`
	SubCode    string        `json:"sub_code,omitempty"`
	ModelKey   string        `json:"model_key,omitempty"`
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	QueueCount int           `json:"queue_count,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("upstream error (%s, status=%d)", e.Kind, e.Status)
}

// NestedQoderDetail captures Qoder's 3-layer nested JSON error payload.
type NestedQoderDetail struct {
	OuterCode         string `json:"outer_code,omitempty"`
	InnerCode         string `json:"inner_code,omitempty"`
	SubCode           string `json:"sub_code,omitempty"`
	IsQueued          bool   `json:"is_queued,omitempty"`
	ModelKey          string `json:"model_key,omitempty"`
	QueueCount        int    `json:"queue_count,omitempty"`
	QueueType         string `json:"queue_type,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	ServiceAvailable  bool   `json:"service_available,omitempty"`
	WaitTime          int    `json:"wait_time,omitempty"`
	PricingURL        string `json:"pricing_url,omitempty"`
}

// Queue runtime defaults and lock-free configurable bounds (aligned with qoder2api).
const (
	DefaultQueueMaxRetries    = 2
	DefaultQueueWaitCapSec    = 30
	DefaultQueueWaitDefSec    = 15
	DefaultQueueWaitBudgetSec = 1800
	maxQueueRetriesCeiling    = 10
	maxQueueWaitCeilingSec    = 300
	maxQueueBudgetCeilingSec  = 86400
	TransientMaxRetries       = 2
)

var (
	queueMu             sync.RWMutex
	QueueModeWait       = true // true = wait/retry inside stream with SSE keepalive; false = fast 503
	QueueMaxRetries     = DefaultQueueMaxRetries
	QueueWaitCap        = DefaultQueueWaitCapSec * time.Second
	QueueWaitDefault    = DefaultQueueWaitDefSec * time.Second
	QueueWaitFirstToken = true
	QueueWaitBudget     = DefaultQueueWaitBudgetSec * time.Second
)

// ApplyQueueSettings updates runtime queue-wait policy without restarting.
func ApplyQueueSettings(waitMode bool, maxRetries, waitCapSec, waitDefSec int, waitFirstToken bool, budgetSec int) {
	if maxRetries <= 0 {
		maxRetries = DefaultQueueMaxRetries
	}
	if maxRetries > maxQueueRetriesCeiling {
		maxRetries = maxQueueRetriesCeiling
	}
	if waitCapSec <= 0 {
		waitCapSec = DefaultQueueWaitCapSec
	}
	if waitCapSec > maxQueueWaitCeilingSec {
		waitCapSec = maxQueueWaitCeilingSec
	}
	if waitDefSec <= 0 {
		waitDefSec = DefaultQueueWaitDefSec
	}
	if waitDefSec > maxQueueWaitCeilingSec {
		waitDefSec = maxQueueWaitCeilingSec
	}
	if budgetSec <= 0 {
		budgetSec = DefaultQueueWaitBudgetSec
	}
	if budgetSec > maxQueueBudgetCeilingSec {
		budgetSec = maxQueueBudgetCeilingSec
	}
	queueMu.Lock()
	QueueModeWait = waitMode
	QueueMaxRetries = maxRetries
	QueueWaitCap = time.Duration(waitCapSec) * time.Second
	QueueWaitDefault = time.Duration(waitDefSec) * time.Second
	QueueWaitFirstToken = waitFirstToken
	QueueWaitBudget = time.Duration(budgetSec) * time.Second
	queueMu.Unlock()
}

// CurrentQueueSettings reads a snapshot of active queue settings.
func CurrentQueueSettings() (waitMode bool, maxRetries int, capD, defD time.Duration, firstTok bool, budget time.Duration) {
	queueMu.RLock()
	defer queueMu.RUnlock()
	return QueueModeWait, QueueMaxRetries, QueueWaitCap, QueueWaitDefault, QueueWaitFirstToken, QueueWaitBudget
}

// ParseQoderNestedError unwraps Qoder's 3-level nested JSON error when present.
func ParseQoderNestedError(detail string) (*NestedQoderDetail, bool) {
	s := strings.TrimSpace(detail)
	if !strings.Contains(s, "subCode") && !strings.Contains(s, "RESOURCE_LIMIT") && !strings.Contains(s, "isQueued") {
		return nil, false
	}
	if idx := strings.IndexByte(s, '{'); idx > 0 {
		s = s[idx:]
	}
	var outer map[string]any
	if json.Unmarshal([]byte(s), &outer) != nil {
		return nil, false
	}
	out := &NestedQoderDetail{OuterCode: anyStr(outer["code"])}
	msg1, _ := outer["message"].(string)
	if msg1 == "" {
		return nil, false
	}
	var inner map[string]any
	if json.Unmarshal([]byte(msg1), &inner) != nil {
		return nil, false
	}
	out.InnerCode = anyStr(inner["code"])
	out.SubCode = anyStr(inner["subCode"])
	if p := anyStr(inner["pricingUrl"]); p != "" {
		out.PricingURL = p
	}
	msg2, _ := inner["message"].(string)
	if msg2 != "" {
		var leaf map[string]any
		if json.Unmarshal([]byte(msg2), &leaf) == nil {
			out.IsQueued, _ = leaf["isQueued"].(bool)
			out.ModelKey = anyStr(leaf["modelKey"])
			out.QueueCount = anyInt(leaf["queueCount"])
			out.QueueType = anyStr(leaf["queueType"])
			out.RetryAfterSeconds = anyInt(leaf["retryAfterSeconds"])
			if sa, ok := leaf["serviceAvailable"].(bool); ok {
				out.ServiceAvailable = sa
			}
			out.WaitTime = anyInt(leaf["waitTime"])
			if sc := anyStr(leaf["subCode"]); sc != "" && out.SubCode == "" {
				out.SubCode = sc
			}
		}
	}
	if out.SubCode == "" && !out.IsQueued && out.RetryAfterSeconds == 0 && out.InnerCode == "" {
		return nil, false
	}
	return out, true
}

func anyStr(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	default:
		return ""
	}
}

func anyInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		return 0
	}
}

var retryAfterRe = regexp.MustCompile(`(?i)"?retryAfterSeconds"?\s*[:=]\s*(\d+)`)

// QueueWaitSeconds extracts retryAfterSeconds/waitTime when a body represents a queued model call.
func QueueWaitSeconds(detail string) int {
	if d, ok := ParseQoderNestedError(detail); ok {
		if d.IsQueued || d.RetryAfterSeconds > 0 {
			w := d.RetryAfterSeconds
			if w <= 0 {
				w = d.WaitTime
			}
			if w <= 0 {
				w = DefaultQueueWaitDefSec
			}
			return w
		}
		return 0
	}
	if strings.Contains(detail, "10605") && (strings.Contains(detail, "isQueued") || strings.Contains(detail, "retryAfterSeconds")) {
		if m := retryAfterRe.FindStringSubmatch(detail); len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n
			}
		}
		return DefaultQueueWaitDefSec
	}
	return 0
}

// ClassifyError maps HTTP status and Qoder body into a structured *Error.
func ClassifyError(status int, body string) *Error {
	ue := &Error{
		Status: status,
		Msg:    strings.TrimSpace(body),
		Kind:   Classify(status, body),
	}
	if d, ok := ParseQoderNestedError(body); ok {
		ue.Code = d.InnerCode
		ue.SubCode = d.SubCode
		ue.ModelKey = d.ModelKey
		ue.QueueCount = d.QueueCount
		if w := d.RetryAfterSeconds; w > 0 {
			ue.RetryAfter = time.Duration(w) * time.Second
		} else if d.WaitTime > 0 {
			ue.RetryAfter = time.Duration(d.WaitTime) * time.Second
		}
	}
	if ue.Kind == ErrQueueWait && ue.RetryAfter <= 0 {
		_, _, waitCap, waitDef, _, _ := CurrentQueueSettings()
		w := time.Duration(QueueWaitSeconds(body)) * time.Second
		if w <= 0 {
			w = waitDef
		}
		if w > waitCap {
			w = waitCap
		}
		ue.RetryAfter = w
	}
	return ue
}

// Classify categorizes an upstream status and response body into an ErrKind.
func Classify(status int, body string) ErrKind {
	low := strings.ToLower(body)

	// 1) Token expired / dead session always takes priority so account refreshes/re-auths promptly.
	for _, m := range []string{
		"token_expire", "tokenexpire", "token expired", "token is expired",
		"credential expired", "invalid_grant", "unauthorized_client",
		"offline user session not found", "12153", "authorization required",
		"unauthenticated", "凭证过期", "令牌过期", "重新授权",
		"token_invalid", "token invalid", "invalid token", "invalid_token", "jwt malformed", "token is invalid", "invalid authorization",
	} {
		if strings.Contains(low, m) {
			return ErrSessionDead
		}
	}
	if status == http.StatusUnauthorized {
		return ErrSessionDead
	}

	// 2) Qoder nested RESOURCE_LIMIT / 10605 queue wait.
	if d, ok := ParseQoderNestedError(body); ok {
		if d.IsQueued || d.RetryAfterSeconds > 0 {
			return ErrQueueWait
		}
		if d.SubCode == "RESOURCE_LIMIT" && !d.ServiceAvailable {
			return ErrModelBlocked
		}
		if d.InnerCode == "115" || d.InnerCode == "112" {
			return ErrModelBlocked
		}
	}
	if strings.Contains(body, "10605") && (strings.Contains(body, "isQueued") || strings.Contains(body, "retryAfterSeconds")) {
		return ErrQueueWait
	}

	// 3) Daily billing count exceeded (110) or model-specific limit (115 / 112).
	if strings.Contains(body, "Billing daily count exceeded") || containsJSONCode(body, "110") {
		return ErrSoftRate
	}
	if (containsJSONCode(body, "115") || containsJSONCode(body, "112")) &&
		(strings.Contains(low, "quota") || strings.Contains(low, "pricingurl") || strings.Contains(low, "limit") || strings.Contains(low, "credit")) {
		if strings.Contains(low, "model") {
			return ErrModelBlocked
		}
		return ErrHardCredit
	}

	// 4) Hard credit / quota exhausted / payment required.
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	for _, m := range []string{
		"quota_exhausted", "quotaexhausted", "quota exceeded", "insufficient_quota",
		"insufficient quota", "insufficient balance", "credits exhausted", "credit exhausted",
		"out of credits", "no credits", "额度不足", "额度耗尽", "配额耗尽", "积分不足",
	} {
		if strings.Contains(low, m) {
			return ErrHardCredit
		}
	}

	// 5) Account-level trial/auth restrictions vs model entitlements.
	for _, m := range []string{
		"trial_restricted", "trial restricted", "trial limit", "trial expired",
		"trial not activated", "request illegal",
	} {
		if strings.Contains(low, m) {
			return ErrAccountFault
		}
	}
	for _, m := range []string{
		"model_not_allowed", "model not allowed", "model not available",
		"model unavailable", "not entitled to model", "unsupported model",
		"model_not_in_plan", "resource_limit",
	} {
		if strings.Contains(low, m) {
			return ErrModelBlocked
		}
	}

	// 6) Content inspection / moderation (do not blame account).
	for _, m := range []string{
		"datainspectionfailed", "inappropriate content", "input text data may contain",
		"contentfilter", "sensitivecontent", "blocked by security policy",
		"unapproved channel", "illegal api invocation",
	} {
		if strings.Contains(low, m) {
			return ErrContentBlocked
		}
	}

	// 7) Prompt length / image / bad parameter request faults (client problem, do not blame account).
	if (status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusRequestEntityTooLarge) &&
		(strings.Contains(low, "prompt is too long") || containsJSONCode(body, "11115") || strings.Contains(low, "context_length_exceeded") || strings.Contains(low, "max_input_tokens")) {
		return ErrPromptTooLong
	}
	for _, m := range []string{
		"invalid image_url", "invalid_image_data", "replace the image", "image conversion", "11135",
	} {
		if strings.Contains(low, m) {
			return ErrImageInvalid
		}
	}
	for _, m := range []string{
		"invalid_parameter_error", "invalid_request_error", "unmarshal chat params failed",
		`"range of `, `"code":11101`, "missing required field",
	} {
		if strings.Contains(low, m) {
			return ErrBadParams
		}
	}

	// 8) Rate limit / 429.
	if status == http.StatusTooManyRequests || strings.Contains(low, "too many requests") || strings.Contains(low, "rate limit") || strings.Contains(low, "rate-limited") || strings.Contains(low, "限流") {
		return ErrSoftRate
	}

	// 9) WAF HTML challenge / empty/challenge 403 block.
	if status == http.StatusForbidden {
		return ErrWafBlock
	}

	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 400 && status < 500 && status != 418 {
		if strings.Contains(low, "provider_error") {
			return ErrServer
		}
		return ErrClient
	}
	if status == 418 || status >= 500 || status == 0 {
		return ErrServer
	}
	return ErrNone
}

func containsJSONCode(body, code string) bool {
	return strings.Contains(body, `"code":`+code) ||
		strings.Contains(body, `"code": `+code) ||
		strings.Contains(body, `"code":"`+code+`"`) ||
		strings.Contains(body, `"code": "`+code+`"`)
}

// IsTransientUpstream determines whether an HTTP failure can be retried once on the same account.
func IsTransientUpstream(status int, body string) bool {
	if status == 401 || status == 403 || status == 429 {
		return false
	}
	kind := Classify(status, body)
	switch kind {
	case ErrContentBlocked, ErrPromptTooLong, ErrImageInvalid, ErrBadParams, ErrClient, ErrModelBlocked, ErrHardCredit, ErrSessionDead:
		return false
	}
	if status == 418 || status >= 500 {
		return true
	}
	if status >= 400 && status < 500 && strings.Contains(strings.ToLower(body), "provider_error") {
		return true
	}
	return false
}

// IsTransientTransport reports recoverable network/TLS/timeout errors.
func IsTransientTransport(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	low := strings.ToLower(err.Error())
	for _, non := range []string{"unsupported protocol scheme", "invalid url", "unknown scheme", "x509:"} {
		if strings.Contains(low, non) {
			return false
		}
	}
	for _, k := range []string{"ssl", "tls", "eof", "reset", "timed out", "timeout", "deadline exceeded", "broken pipe", "connection refused", "connection reset"} {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// IsModelRateLimit indicates an upstream rate/quota error scoped to a single model rather than the whole account.
func IsModelRateLimit(body string) bool {
	low := strings.ToLower(body)
	if strings.Contains(low, "model") && (strings.Contains(low, "limit") || strings.Contains(low, "115") || strings.Contains(low, "112") || strings.Contains(low, "resource_limit")) {
		return true
	}
	if d, ok := ParseQoderNestedError(body); ok && d.ModelKey != "" {
		return true
	}
	return false
}

// ParseRateReset parses a reset timestamp from upstream body (e.g. billing daily limit reset at midnight CST).
func ParseRateReset(body string) (time.Time, bool) {
	if strings.Contains(body, "Billing daily count exceeded") || containsJSONCode(body, "110") {
		cst := time.FixedZone("CST", 8*3600)
		now := time.Now().In(cst)
		nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 5, 0, cst)
		return nextMidnight, true
	}
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) == nil {
		for _, k := range []string{"reset_at", "resetAt", "retry_at", "retryAt"} {
			if raw, ok := m[k]; ok {
				if sec, ok := parseEpochFlex(raw); ok && sec > time.Now().Unix() {
					return time.Unix(sec, 0), true
				}
			}
		}
	}
	return time.Time{}, false
}

// IsAlreadyCheckin reports whether a check-in error indicates today's campaign is already claimed.
func IsAlreadyCheckin(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "already") || strings.Contains(low, "已领取") || strings.Contains(low, "已签到") || strings.Contains(low, "replayed")
}

// HintContext carries request capability context for GatewayHint.
type HintContext struct {
	Model               string
	HasImage            bool
	ModelSupportsImages bool
	ModelInCatalog      bool
}

const noHealthyHint = "no healthy account available in pool; check /status or retry later"

// NoHealthyAccountHint returns the gateway_hint for pool exhaustion errors.
func NoHealthyAccountHint() string { return noHealthyHint }

// GatewayHint provides a non-intrusive diagnostic hint alongside upstream error messages.
func GatewayHint(kind ErrKind, msg string, ctx HintContext) string {
	low := strings.ToLower(msg)
	if ctx.HasImage && ctx.ModelInCatalog && !ctx.ModelSupportsImages &&
		(strings.Contains(low, "image") || strings.Contains(low, "vision") || kind == ErrBadParams) {
		return "model " + ctx.Model + " does not support images; pick one with supports_images=true from /v1/models"
	}
	switch kind {
	case ErrQueueWait:
		return "upstream model queue busy (10605 / RESOURCE_LIMIT); retry shortly or switch models"
	case ErrPromptTooLong:
		return "request context exceeds the model's limit; reduce history/message size"
	case ErrImageInvalid:
		return "image request was rejected by upstream; check image_url format and image data"
	case ErrWafBlock:
		return "upstream WAF blocked the gateway; retry after the block window"
	case ErrSoftRate:
		return "rate limited by upstream; retry after reset"
	case ErrAccountFault:
		return "account-level restriction or fault at upstream; gateway will rotate or pause this account"
	case ErrSessionDead:
		return "account session expired (TOKEN_EXPIRE); account disabled until re-login"
	case ErrHardCredit:
		return "account credits exhausted at upstream; waiting for daily check-in or quota renewal"
	case ErrModelBlocked:
		return "requested model is unavailable on this account or realm; switch model or account"
	case ErrContentBlocked:
		return "request content was rejected by content policy; adjust the prompt and retry"
	case ErrBadParams:
		return "request parameters were rejected; check message format, tools, or max_tokens"
	default:
		return ""
	}
}

// FrameHintFunc returns an SSE error-frame hint resolver.
func FrameHintFunc(ctxFn func() HintContext) func(string) string {
	return func(payload string) string {
		if payload == "" || payload == "[DONE]" {
			return ""
		}
		k := Classify(0, payload)
		return GatewayHint(k, payload, ctxFn())
	}
}

// FriendlyError turns an upstream/pool error into a readable message and machine error code.
func FriendlyError(err error) (string, string) {
	if err == nil {
		return "", ""
	}
	var ue *Error
	if errors.As(err, &ue) {
		if d, ok := ParseQoderNestedError(ue.Msg); ok {
			model := d.ModelKey
			if model == "" {
				model = "(unspecified)"
			}
			if d.IsQueued || d.RetryAfterSeconds > 0 {
				return fmt.Sprintf("Model %s is queued upstream (queue=%d, retryAfter=%ds)", model, d.QueueCount, d.RetryAfterSeconds), string(ue.Kind)
			}
			if !d.ServiceAvailable {
				return fmt.Sprintf("Model %s is not available for this account (%s)", model, d.SubCode), string(ue.Kind)
			}
		}
		return ue.Error(), string(ue.Kind)
	}
	return err.Error(), "qoder_error"
}

func parseEpochFlex(raw any) (int64, bool) {
	switch v := raw.(type) {
	case float64:
		if v > 1e12 {
			v /= 1000
		}
		return int64(v), v > 0
	case int64:
		if v > 1e12 {
			v /= 1000
		}
		return v, v > 0
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		if f > 1e12 {
			f /= 1000
		}
		return int64(f), f > 0
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			if f > 1e12 {
				f /= 1000
			}
			return int64(f), f > 0
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Unix(), true
		}
		if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}
