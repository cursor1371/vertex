package transport

import (
	"crypto/sha1"
	"encoding/base64"
	"strings"

	http "github.com/bogdanfinn/fhttp"

	"github.com/bsfdsagfadg/vertex/internal/infra/fingerprint"
)

// bundleOf 返回当前生效的指纹包：开关关闭/未设置时回退既有 Chrome 150 头基线
// （与改动前逐字节一致；TLS 侧仍由 client.go 维持 144/146 随机，不消费该包的
// TLS 池）。平台描述与地区语言字段（arch/bitness/wow64 等）也来自本包，
// 与 platform/Accept-Language 同源自洽。
func bundleOf() fingerprint.Bundle {
	return fingerprint.ActiveOrLegacy()
}

// FingerprintHL 返回当前生效的地区语言 reCAPTCHA hl 值（与 Accept-Language 同源）。
// 供 recaptcha 包读取，使其不必直接依赖 fingerprint 包。
func FingerprintHL() string { return bundleOf().RecaptchaHl }

// Google 侧魔数常量：语义绑定 Cloud Console 内部协议，暂不开放配置
// （改错即失效），与平台/地区描述类字段分开。
const (
	xBrowserChannel   = "stable"
	xBrowserCopyright = "Copyright 2026 Google LLC. All Rights Reserved."
	xGoogExtJspb      = "[null,null,null,194274]"
)

// XHRHeaders 构造 XHR/fetch 风格的完整 Chrome 请求头（含 H2 头顺序 HeaderOrderKey）。
//
// tls-client 不像 curl_cffi 那样自动补 sec-ch-ua / sec-fetch-* 等头，必须手动设全，
// 否则 Google 匿名端点的指纹校验不通过。头值取自当前生效指纹包（chrome_150 逐字节
// 抄自经实测的 PoC——recaptcha reload 与 batchGraphql 都用此头）。
func XHRHeaders(contentType, accept, origin, referer, site string) http.Header {
	p := bundleOf()
	h := http.Header{
		"sec-ch-ua":                   {p.SecCHUA},
		"sec-ch-ua-mobile":            {p.SecCHUAMobile},
		"sec-ch-ua-platform":          {p.SecCHUAPlatform},
		"sec-ch-ua-arch":              {p.SecCHUAArch},
		"sec-ch-ua-bitness":           {p.SecCHUABitness},
		"sec-ch-ua-full-version":      {p.SecCHUAFullVersion},
		"sec-ch-ua-full-version-list": {p.SecCHUAFullVersionList},
		"sec-ch-ua-platform-version":  {p.SecCHUAPlatformVersion},
		"sec-ch-ua-model":             {p.SecCHUAModel},
		"sec-ch-ua-wow64":             {p.SecCHUAWow64},
		"sec-ch-ua-form-factors":      {p.SecCHUAFormFactors},
		"user-agent":                  {p.UA},
		"accept":                      {accept},
		"origin":                      {origin},
		"sec-fetch-site":              {site},
		"sec-fetch-mode":              {"cors"},
		"sec-fetch-dest":              {"empty"},
		"referer":                     {referer},
		"accept-encoding":             {"gzip, deflate, br"},
		"accept-language":             {p.AcceptLanguage},
		"priority":                    {"u=1, i"},
		"x-goog-authuser":             {"0"},
		"x-browser-channel":           {xBrowserChannel},
		"x-browser-copyright":         {xBrowserCopyright},
		"x-browser-validation":        {GenerateXBrowserValidation(p.UA)},
		"x-browser-year":              {p.XBrowserYear},
		"x-goog-ext-353267353-jspb":   {xGoogExtJspb},
		http.HeaderOrderKey: {
			"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "sec-ch-ua-arch", "sec-ch-ua-bitness",
			"sec-ch-ua-full-version", "sec-ch-ua-full-version-list", "sec-ch-ua-platform-version",
			"sec-ch-ua-model", "sec-ch-ua-wow64", "sec-ch-ua-form-factors",
			"user-agent", "content-type", "accept", "x-goog-authuser", "x-browser-channel",
			"x-browser-copyright", "x-browser-validation", "x-browser-year", "x-goog-ext-353267353-jspb",
			"origin", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest",
			"referer", "accept-encoding", "accept-language", "priority",
		},
	}
	if contentType != "" {
		h["content-type"] = []string{contentType}
	}
	return h
}

// AnchorHeaders 构造 recaptcha anchor iframe 导航请求头。布局逐字节对齐 PoC，
// 版本相关头值取自当前生效指纹包。
func AnchorHeaders() http.Header {
	p := bundleOf()
	return http.Header{
		"sec-ch-ua":                   {p.SecCHUA},
		"sec-ch-ua-mobile":            {p.SecCHUAMobile},
		"sec-ch-ua-platform":          {p.SecCHUAPlatform},
		"sec-ch-ua-arch":              {p.SecCHUAArch},
		"sec-ch-ua-bitness":           {p.SecCHUABitness},
		"sec-ch-ua-full-version":      {p.SecCHUAFullVersion},
		"sec-ch-ua-full-version-list": {p.SecCHUAFullVersionList},
		"sec-ch-ua-platform-version":  {p.SecCHUAPlatformVersion},
		"sec-ch-ua-model":             {p.SecCHUAModel},
		"sec-ch-ua-wow64":             {p.SecCHUAWow64},
		"sec-ch-ua-form-factors":      {p.SecCHUAFormFactors},
		"upgrade-insecure-requests":   {"1"},
		"user-agent":                  {p.UA},
		"accept":                      {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
		"sec-fetch-site":              {"cross-site"},
		"sec-fetch-mode":              {"navigate"},
		"sec-fetch-dest":              {"iframe"},
		"accept-encoding":             {"gzip, deflate, br"},
		"accept-language":             {p.AcceptLanguage},
		http.HeaderOrderKey: {
			"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "sec-ch-ua-arch", "sec-ch-ua-bitness",
			"sec-ch-ua-full-version", "sec-ch-ua-full-version-list", "sec-ch-ua-platform-version",
			"sec-ch-ua-model", "sec-ch-ua-wow64", "sec-ch-ua-form-factors",
			"upgrade-insecure-requests",
			"user-agent", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest",
			"accept-encoding", "accept-language",
		},
	}
}

// GenerateXBrowserValidation calculates the x-browser-validation header based on User-Agent.
func GenerateXBrowserValidation(ua string) string {
	apiKey := "AIzaSyA2KlwBX3mkFo30om9LUFYQhpqLoa_BNhE" // Default Windows key
	uaLower := strings.ToLower(ua)
	if strings.Contains(uaLower, "linux") {
		apiKey = "AIzaSyBqJZh-7pA44blAaAkH6490hUFOwX0KCYM"
	} else if strings.Contains(uaLower, "macintosh") || strings.Contains(uaLower, "mac os x") {
		apiKey = "AIzaSyDr2UxVnv_U85AbhhY8XSHSIavUW0DC-sY"
	}
	h := sha1.New()
	h.Write([]byte(apiKey + ua))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
